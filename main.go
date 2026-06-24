package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	texttmpl "text/template"
	"time"

	htmltmpl "html/template"
)

// Config holds SMTP settings
type Config struct {
	Host              string `json:"host"`
	Port              int    `json:"port"`
	User              string `json:"user"`
	Password          string `json:"password"`
	From              string `json:"from"`
	SenderName        string `json:"sender_name"` // Display name in From header
	AuthType          string `json:"auth_type"`     // PLAIN or LOGIN
	APIKey            string `json:"api_key"`       // AgentForms API key for tier lookup
	Tier              string `json:"tier"`          // Cached tier from API
	FromDomain        string `json:"from_domain"`   // Custom from domain (Phase 1)
	RelayHost         string `json:"relay_host"`    // Custom relay host (Phase 1)
	WebhookURL        string `json:"webhook_url"`   // Default webhook URL (Phase 2)
	MaxSendsPerHour   int    `json:"max_sends_per_hour"`   // 0 = unlimited (Rate Limiting)
	MaxAttachmentSize int64  `json:"max_attachment_size"`  // bytes, 0 = unlimited (default 25MB)
}

// SentEntry records a sent email
type SentEntry struct {
	To             []string `json:"to"`
	Subject        string   `json:"subject"`
	Body           string   `json:"body"`
	Files          []string `json:"files"`
	Timestamp      string   `json:"timestamp"`
	Status         string   `json:"status"`
	Error          string   `json:"error,omitempty"`
	WebhookURL     string   `json:"webhook_url,omitempty"`        // Phase 2
	WebhookStatus  string   `json:"webhook_status,omitempty"`     // Phase 2: pending/sent/failed
	WebhookAttempts int     `json:"webhook_attempts,omitempty"`   // Phase 2
	TrackingToken  string   `json:"tracking_token,omitempty"`     // Phase 4
	OpenCount      int      `json:"open_count,omitempty"`         // Phase 4
	LastOpened     string   `json:"last_opened,omitempty"`        // Phase 4
	BounceCount    int      `json:"bounce_count,omitempty"`       // Phase 4
	BouncedAt      string   `json:"bounced_at,omitempty"`         // Phase 4
	UniqueOpens    []string `json:"unique_opens,omitempty"`       // Phase 4: recipient IPs
}

// webhookPayload is sent to callback webhooks after successful send
type webhookPayload struct {
	Event    string   `json:"event"`
	Token    string   `json:"token"`
	To       []string `json:"to"`
	Subject  string   `json:"subject"`
	Timestamp string  `json:"timestamp"`
	Status   string   `json:"status"`
}

// EmailTemplate stores a named email template
type EmailTemplate struct {
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Body    string `json:"body"`       // text body
	HTML    string `json:"html,omitempty"` // html body
	Vars    []string `json:"vars,omitempty"` // variable names
	Updated string `json:"updated"`
}

// smtpClient abstracts the unexported *smtp.Client
type smtpClient interface {
	Close() error
	Extension(string) (bool, string)
	StartTLS(*tls.Config) error
	Auth(smtp.Auth) error
	Mail(string) error
	Rcpt(string) error
	Data() (io.WriteCloser, error)
	Quit() error
}

// TierCache stores the result of a tier check with timestamp
type TierCache struct {
	Tier     string    `json:"tier"`
	Checked  time.Time `json:"checked"`
}

const (
	configDir      = ".config/agent-relay"
	stateDir       = ".local/state/agent-relay"
	configFile     = "config.json"
	sentFile       = "sent.json"
	tierCacheFile  = "tier_cache.json"
	templatesFile  = "templates.json" // Phase 3
	agentFormsURL  = "https://agentforms.io"
	tierCacheTTL   = 6 * time.Hour // Cache for 6 hours
	relayFooter    = "\n\n--\nSent via AgentRelay | More at agentforms.io"
	// Rate limit defaults
	defaultFreeRateLimit  = 30 // sends/hour for free tier
	defaultPaidRateLimit  = 100 // sends/hour for paid tier
	defaultMaxAttachment  = 25 * 1024 * 1024 // 25MB
	defaultMaxMessageSize = 35 * 1024 * 1024 // 35MB total
)

// RateLimiter tracks send timestamps per key using a sliding window
type RateLimiter struct {
	mu       sync.Mutex
	sends    map[string][]time.Time // key -> timestamps within the window
	limit    int                    // max sends per hour
	window   time.Duration          // sliding window (1 hour)
	cleanup  time.Duration          // how often to prune old entries
	lastClean time.Time
}

// NewRateLimiter creates a rate limiter
func NewRateLimiter(limit int) *RateLimiter {
	return &RateLimiter{
		sends:   make(map[string][]time.Time),
		limit:   limit,
		window:  time.Hour,
		cleanup: 30 * time.Minute,
	}
}

// Allow checks if a send is allowed for the given key
func (rl *RateLimiter) Allow(key string) error {
	if rl.limit <= 0 {
		return nil // unlimited
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Prune old entries (every 30 min)
	if now.Sub(rl.lastClean) > rl.cleanup {
		for k, timestamps := range rl.sends {
			var valid []time.Time
			for _, t := range timestamps {
				if t.After(cutoff) {
					valid = append(valid, t)
				}
			}
			if len(valid) == 0 {
				delete(rl.sends, k)
			} else {
				rl.sends[k] = valid
			}
		}
		rl.lastClean = now
	}

	timestamps := rl.sends[key]
	if len(timestamps) >= rl.limit {
		return fmt.Errorf("rate limit exceeded: %d sends/hour (try again in %s)",
			rl.limit, timestamps[0].Add(rl.window).Sub(now).Round(time.Second))
	}

	rl.sends[key] = append(timestamps, now)
	return nil
}

var (
	// Global rate limiter instance (initialized on first send)
	rateLimiter *RateLimiter
	rlOnce      sync.Once
)

// getRateLimiter returns the global rate limiter, initialized per-tier
func getRateLimiter(cfg *Config) *RateLimiter {
	var rl *RateLimiter
	rlOnce.Do(func() {
		limit := cfg.MaxSendsPerHour
		if limit == 0 {
			// Determine limit from tier
			if isPaidTier(cfg) {
				limit = defaultPaidRateLimit
			} else {
				limit = defaultFreeRateLimit
			}
		}
		rl = NewRateLimiter(limit)
	})
	return rl
}

// rateLimitKey generates a rate limit key based on config
func rateLimitKey(cfg *Config) string {
	// Use API key if available, otherwise From address as fallback
	if cfg.APIKey != "" {
		return "apikey:" + cfg.APIKey
	}
	return "from:" + cfg.From
}

// Default managed relay config (AgentForms Hetzner server)
// Password is injected at build time via -ldflags.
var relayToken = "AGENTFORMS_RELAY_TOKEN" // replaced at build time

var defaultConfig = Config{
	Host:     "mail.agentforms.io",
	Port:     587,
	User:     "agentforms",
	Password: relayToken,
	From:     "noreply@agentforms.io",
	AuthType: "PLAIN",
}

func homeDir() string {
	return os.Getenv("HOME")
}

func configPath() string {
	return filepath.Join(homeDir(), configDir, configFile)
}

func sentPath() string {
	return filepath.Join(homeDir(), stateDir, sentFile)
}

func templatesPath() string {
	return filepath.Join(homeDir(), configDir, templatesFile)
}

func loadConfig() (*Config, error) {
	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		// No config file — use default managed relay (zero-config)
		return &defaultConfig, nil
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

func configExists() bool {
	_, err := os.Stat(configPath())
	return err == nil
}

func usingDefaults() bool {
	return !configExists()
}

func saveConfig(cfg *Config) error {
	dir := filepath.Dir(configPath())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}

func loadSentLog() ([]SentEntry, error) {
	path := sentPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []SentEntry{}, nil
		}
		return nil, err
	}
	var entries []SentEntry
	json.Unmarshal(data, &entries)
	return entries, nil
}

func appendSentLog(entry SentEntry) error {
	dir := filepath.Dir(sentPath())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	entries, _ := loadSentLog()
	entries = append(entries, entry)
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(sentPath(), data, 0600)
}

// ─── Tier cache ────────────────────────────────────────────────────────────

func tierCachePath() string {
	return filepath.Join(homeDir(), stateDir, tierCacheFile)
}

func loadTierCache() *TierCache {
	data, err := os.ReadFile(tierCachePath())
	if err != nil {
		return nil
	}
	var cache TierCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil
	}
	if time.Since(cache.Checked) > tierCacheTTL {
		return nil // expired
	}
	return &cache
}

func saveTierCache(cache TierCache) error {
	dir := filepath.Dir(tierCachePath())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tierCachePath(), data, 0600)
}

// checkTier queries AgentForms for the user's tier, using local cache if fresh
func checkTier(apiKey string) string {
	if apiKey == "" {
		return "free"
	}

	// Check cache first
	if cache := loadTierCache(); cache != nil {
		return cache.Tier
	}

	// Hit the API
	resp, err := http.Get(fmt.Sprintf("%s/api/v2/verify?key=%s", agentFormsURL, apiKey))
	if err != nil {
		return "free" // Default to footer on error
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "free"
	}

	var result struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "free"
	}

	// Cache the result
	saveTierCache(TierCache{
		Tier:    result.Tier,
		Checked: time.Now(),
	})

	return result.Tier
}

// shouldShowFooter returns true if the relay footer should be appended
func shouldShowFooter(cfg *Config) bool {
	tier := checkTier(cfg.APIKey)
	return tier == "free"
}

// ─── Phase 1 helpers ───────────────────────────────────────────────────────

// isPaidTier returns true if the user's tier is not "free"
func isPaidTier(cfg *Config) bool {
	if cfg.Tier != "" {
		return cfg.Tier != "free"
	}
	return checkTier(cfg.APIKey) != "free"
}

// resolveFromAddress extracts the local part from config.From and swaps the domain
// If fromDomain is set, returns localPart@fromDomain; otherwise returns cfg.From
func resolveFromAddress(cfg *Config, fromDomain string) string {
	if fromDomain == "" && cfg.FromDomain == "" {
		return cfg.From
	}
	domain := fromDomain
	if domain == "" {
		domain = cfg.FromDomain
	}
	parts := strings.SplitN(cfg.From, "@", 2)
	if len(parts) != 2 {
		return cfg.From
	}
	return parts[0] + "@" + domain
}

// ─── Phase 2: Webhooks ─────────────────────────────────────────────────────

func sendWebhook(url string, payload webhookPayload) string {
	data, err := json.Marshal(payload)
	if err != nil {
		return "marshal error"
	}

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			time.Sleep(backoff) // 1s, 2s exponential backoff
		}

		req, err := http.NewRequest("POST", url, bytes.NewReader(data))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return "sent"
		}
	}
	return "failed"
}

// ─── Phase 3: Templates ────────────────────────────────────────────────────

func loadTemplates() ([]EmailTemplate, error) {
	path := templatesPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []EmailTemplate{}, nil
		}
		return nil, err
	}
	var templates []EmailTemplate
	if err := json.Unmarshal(data, &templates); err != nil {
		return nil, fmt.Errorf("invalid templates: %w", err)
	}
	return templates, nil
}

func saveTemplates(templates []EmailTemplate) error {
	dir := filepath.Dir(templatesPath())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(templates, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(templatesPath(), data, 0644)
}

// mergeTemplates merges remote templates into local, preserving local overrides
func mergeTemplates(local, remote []EmailTemplate) []EmailTemplate {
	localMap := make(map[string]*EmailTemplate)
	for i := range local {
		localMap[local[i].Name] = &local[i]
	}

	var result []EmailTemplate
	seen := make(map[string]bool)

	// Keep existing local templates
	for _, t := range local {
		seen[t.Name] = true
		result = append(result, t)
	}

	// Add new remote templates
	for _, t := range remote {
		if !seen[t.Name] {
			result = append(result, t)
		}
	}

	return result
}

// renderTemplate renders an email template using html/template with provided variables
func renderTemplate(tmplStr string, vars map[string]string) (string, error) {
	t, err := texttmpl.New("email").Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("template parse error: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("template render error: %w", err)
	}
	return buf.String(), nil
}

func cmdTemplates(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: relay templates <subcommand> [options]")
		fmt.Println("Subcommands: sync, list, show, add")
		return
	}

	switch args[0] {
	case "list", "ls":
		cmdTemplatesList()
	case "show":
		if len(args) < 2 {
			fmt.Println("Usage: relay templates show <name>")
			return
		}
		cmdTemplatesShow(args[1])
	case "add":
		if len(args) < 2 {
			fmt.Println("Usage: relay templates add <name>")
			fmt.Println("  Opens editor to create/edit a template.")
			return
		}
		cmdTemplatesAdd(args[1])
	case "sync":
		cmdTemplatesSync()
	default:
		fmt.Printf("Unknown subcommand: %s\n", args[0])
	}
}

func cmdTemplatesList() {
	templates, err := loadTemplates()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	if len(templates) == 0 {
		fmt.Println("No templates found. Use 'relay templates sync' to fetch from AgentForms.")
		return
	}
	fmt.Printf("=== Templates (%d) ===\n\n", len(templates))
	for _, t := range templates {
		fmt.Printf("  %-20s  %s\n", t.Name, t.Subject)
	}
}

func cmdTemplatesShow(name string) {
	templates, err := loadTemplates()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	for _, t := range templates {
		if t.Name == name {
			fmt.Printf("=== Template: %s ===\n\n", t.Name)
			fmt.Printf("Subject: %s\n", t.Subject)
			if len(t.Vars) > 0 {
				fmt.Printf("Variables: %s\n", strings.Join(t.Vars, ", "))
			}
			fmt.Printf("\nBody:\n%s\n", t.Body)
			if t.HTML != "" {
				fmt.Printf("\nHTML:\n%s\n", t.HTML)
			}
			fmt.Printf("\nUpdated: %s\n", t.Updated)
			return
		}
	}
	fmt.Printf("Template '%s' not found.\n", name)
}

func cmdTemplatesAdd(name string) {
	templates, _ := loadTemplates()

	// Check if template exists
	var idx = -1
	for i, t := range templates {
		if t.Name == name {
			idx = i
			break
		}
	}

	var tmpl EmailTemplate
	if idx >= 0 {
		tmpl = templates[idx]
	} else {
		tmpl = EmailTemplate{
			Name:    name,
			Updated: time.Now().Format(time.RFC3339),
		}
	}

	fmt.Println("Enter template fields (empty to skip):")

	fmt.Printf("Subject [%s]: ", tmpl.Subject)
	var s string
	fmt.Scanln(&s)
	if s != "" {
		tmpl.Subject = s
	}

	// Read body with bufio for multi-line
	fmt.Printf("Body [%s]: ", tmpl.Body)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	body := scanner.Text()
	if body != "" {
		tmpl.Body = body
	}

	// Simple vars extraction from template
	tmpl.Vars = extractVars(tmpl.Subject + tmpl.Body + tmpl.HTML)
	tmpl.Updated = time.Now().Format(time.RFC3339)

	if idx >= 0 {
		templates[idx] = tmpl
	} else {
		templates = append(templates, tmpl)
	}

	if err := saveTemplates(templates); err != nil {
		fmt.Printf("Error saving: %v\n", err)
		return
	}
	fmt.Printf("✓ Template '%s' saved.\n", name)
}

func cmdTemplatesSync() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		return
	}

	if cfg.APIKey == "" {
		fmt.Println("Not linked to AgentForms. Run 'relay link' first.")
		return
	}

	// Fetch template list with auth
	listURL := fmt.Sprintf("%s/api/v2/templates", agentFormsURL)
	listReq, err := http.NewRequest("GET", listURL, nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	listReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	listReq.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(listReq)
	if err != nil {
		fmt.Printf("Error fetching templates: %v\n", err)
		fmt.Println("Using offline mode — sync will retry later.")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Server returned %d — templates unavailable.\n", resp.StatusCode)
		if resp.StatusCode == 401 {
			fmt.Println("Invalid API key. Run 'relay unlink' then 'relay link' to re-authenticate.")
		}
		return
	}

	var apiResp struct {
		Builtin []json.RawMessage `json:"builtin_templates"`
		Custom  []json.RawMessage `json:"custom_templates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		fmt.Printf("Error parsing templates: %v\n", err)
		return
	}

	// Fetch content for each custom template
	var remote []EmailTemplate
	for _, raw := range apiResp.Custom {
		var t struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Updated string `json:"updated_at"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}

		// Fetch template content
		contentURL := fmt.Sprintf("%s/api/v2/templates/%s/content", agentFormsURL, t.Name)
		contentReq, _ := http.NewRequest("GET", contentURL, nil)
		contentReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		contentReq.Header.Set("Accept", "application/json")

		contentResp, err := client.Do(contentReq)
		if err != nil {
			fmt.Printf("  Warning: could not fetch %s: %v\n", t.Name, err)
			continue
		}

		var contentData struct {
			Content string `json:"content"`
			Name    string `json:"name"`
		}
		if err := json.NewDecoder(contentResp.Body).Decode(&contentData); err != nil {
			contentResp.Body.Close()
			fmt.Printf("  Warning: could not parse %s content\n", t.Name)
			continue
		}
		contentResp.Body.Close()

		// Extract variables from content
		vars := extractVars(contentData.Content)

		remote = append(remote, EmailTemplate{
			Name:    t.Name,
			Subject: "Template: " + t.Name,
			Body:    contentData.Content,
			HTML:    contentData.Content,
			Vars:    vars,
			Updated: t.Updated,
		})
	}

	local, _ := loadTemplates()
	merged := mergeTemplates(local, remote)
	if err := saveTemplates(merged); err != nil {
		fmt.Printf("Error saving: %v\n", err)
		return
	}
	fmt.Printf("✓ Synced %d templates (local: %d, remote: %d, total: %d)\n",
		len(remote), len(local), len(remote), len(merged))
}

func extractVars(s string) []string {
	var vars []string
	seen := make(map[string]bool)
	// Simple {{.VarName}} extraction
	parts := strings.Split(s, "{{.")
	for _, p := range parts[1:] {
		end := strings.Index(p, "}}")
		if end >= 0 {
			v := strings.TrimSpace(p[:end])
			if v != "" && !seen[v] {
				seen[v] = true
				vars = append(vars, v)
			}
		}
	}
	return vars
}

// ─── Phase 4: Delivery Tracking ────────────────────────────────────────────

func generateTrackingToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// injectOpenPixel injects a 1x1 tracking pixel into HTML body
func injectOpenPixel(htmlBody, token string) string {
	pixel := fmt.Sprintf(`<img src="http://localhost:5090/track/open/%s" width="1" height="1" style="display:none"/>`, token)
	// Inject before </body> if present, else append
	if strings.Contains(strings.ToLower(htmlBody), "</body>") {
		return strings.Replace(htmlBody, "</body>", pixel+"\\n</body>", 1)
	}
	return htmlBody + pixel
}

func recordOpen(token string) bool {
	entries, err := loadSentLog()
	if err != nil {
		return false
	}
	for i := range entries {
		if entries[i].TrackingToken == token {
			entries[i].OpenCount++
			entries[i].LastOpened = time.Now().Format(time.RFC3339)
			_ = appendSentLog(entries[i])
			return true
		}
	}
	return false
}

func recordBounce(token string) bool {
	entries, err := loadSentLog()
	if err != nil {
		return false
	}
	for i := range entries {
		if entries[i].TrackingToken == token {
			entries[i].BounceCount++
			entries[i].BouncedAt = time.Now().Format(time.RFC3339)
			_ = appendSentLog(entries[i])
			return true
		}
	}
	return false
}

// ─── End of new functions ──────────────────────────────────────────────────

// cmdLink links the relay to an AgentForms account via API key
func cmdLink() {
	fmt.Println("=== Link AgentRelay to AgentForms ===")
	fmt.Println()
	fmt.Println("Enter your AgentForms API key (starts with afk_live_)")
	fmt.Println("You can find it at agentforms.io/settings/api-keys")
	fmt.Println()

	fmt.Print("API key: ")
	var apiKey string
	fmt.Scanln(&apiKey)
	apiKey = strings.TrimSpace(apiKey)

	if apiKey == "" || len(apiKey) < 12 {
		fmt.Println("Error: invalid API key")
		os.Exit(1)
	}

	// Verify the key by calling the verify endpoint
	resp, err := http.Get(fmt.Sprintf("%s/api/v2/verify?key=%s", agentFormsURL, apiKey))
	if err != nil {
		fmt.Printf("Error connecting to AgentForms: %v\n", err)
		fmt.Println("Make sure you have internet access and agentforms.io is reachable.")
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		fmt.Println("Error: invalid API key. Check that it starts with 'afk_live_' and try again.")
		os.Exit(1)
	}

	var result struct {
		Tier       string `json:"tier"`
		KeyPrefix  string `json:"key_prefix"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("Error parsing response: %v\n", err)
		os.Exit(1)
	}

	// Save key to config
	cfg, _ := loadConfig()
	cfg.APIKey = apiKey
	cfg.Tier = result.Tier

	if err := saveConfig(cfg); err != nil {
		fmt.Printf("Error saving config: %v\n", err)
		os.Exit(1)
	}

	// Cache the tier
	saveTierCache(TierCache{
		Tier:    result.Tier,
		Checked: time.Now(),
	})

	fmt.Printf("\n✓ Linked to AgentForms\n")
	fmt.Printf("  Key: %s...\n", result.KeyPrefix)
	fmt.Printf("  Tier: %s\n", result.Tier)

	if result.Tier == "free" {
		fmt.Println("\n  Note: Free tier includes a relay footer on sent emails.")
		fmt.Println("  Upgrade at agentforms.io to remove it.")
	} else {
		fmt.Println("\n  Relay footer removed (paid tier).")
	}
	fmt.Println("\n  Run 'relay unlink' to remove this link.")
}

// cmdUnlink removes the AgentForms API key
func cmdUnlink() {
	cfg, _ := loadConfig()
	if cfg.APIKey == "" {
		fmt.Println("Not linked to AgentForms. No key to remove.")
		return
	}

	cfg.APIKey = ""
	cfg.Tier = ""
	if err := saveConfig(cfg); err != nil {
		fmt.Printf("Error saving config: %v\n", err)
		os.Exit(1)
	}

	// Clear tier cache
	os.Remove(tierCachePath())

	fmt.Println("✓ Unlinked from AgentForms")
	fmt.Println("  Relay footer will appear on sent emails.")
}

func printUsage() {
	fmt.Println(`agent-relay - Send files via email from the terminal

Usage:
  relay setup                          Configure SMTP settings
  relay use-defaults                   Reset to AgentForms managed relay
  relay link                           Link to AgentForms account (API key)
  relay unlink                         Remove AgentForms link
  relay send --to ADDR [options]       Send an email
  relay sent [--n 10] [--opens] [--bounces]  View sent history
  relay serve [--port 5090]            Start web server
  relay templates <subcommand>         Manage email templates
  relay metrics                        Show delivery metrics

Send options:
  --to, -t          Recipient (required, comma-separated for multiple)
  --from-name, -F   Sender display name (e.g. "Vincent")
  --bcc, -c         BCC recipients (comma-separated)
  --subject, -s     Email subject (default: "File from relay")
  --body, -b        Plain text body
  --html-body, -B   HTML body (sends multipart/alternative)
  --file, -f        Attach file (can be repeated, supports glob patterns)
  --max-size        Max attachment size (e.g. "10M", "500K", "1g")
  --from-domain     Override from domain (e.g. "mycompany.com") [paid]
  --relay-host      Override relay SMTP host [paid]
  --webhook         Callback URL for delivery notification [paid]
  --template        Use named template (renders subject + body) [paid]
  --vars            Template variables as key=value pairs (repeatable)

Templates subcommands:
  relay templates sync                  Fetch templates from AgentForms
  relay templates list                  List saved templates
  relay templates show <name>           Show template details
  relay templates add <name>            Create/edit a template

Examples:
  relay send --to user@example.com --file ~/report.pdf
  relay send --to a@x.com,b@y.com --subject "Invoice" --file invoice.pdf
  relay send -t user@x.com -F "Vincent" -s "Report" -b "Plain text"
  relay send -t user@x.com --bcc boss@x.com -f ~/data/*.csv --max-size 25M
  relay send -t user@x.com --from-domain mycompany.com --webhook https://hooks.example.com/delivered
  relay send --template welcome --vars name=Alice --vars plan=pro
  relay templates sync
  relay sent --opens --bounces
  relay metrics
  relay link
  relay serve
  relay sent -n 5`)
}

func cmdSetup() {
	fmt.Println("=== Agent Relay Setup ===")
	fmt.Println()
	fmt.Println("1. Use AgentForms managed SMTP (recommended — just works)")
	fmt.Println("2. Configure custom SMTP server")
	fmt.Println()
	fmt.Print("Choose [1]: ")
	var choice string
	fmt.Scanln(&choice)
	if choice == "2" {
		setupCustom()
		return
	}

	// Managed mode — create config pointing to AgentForms
	fmt.Println("\n--- AgentForms Managed Relay ---")
	fmt.Println()

	var senderName string
	fmt.Print("Your name (displayed as sender, leave blank for none): ")
	fmt.Scanln(&senderName)

	cfg := &Config{
		Host:        defaultConfig.Host,
		Port:        defaultConfig.Port,
		From:        defaultConfig.From,
		User:        defaultConfig.User,
		Password:    defaultConfig.Password,
		AuthType:    "PLAIN",
		SenderName:  strings.TrimSpace(senderName),
	}

	if err := saveConfig(cfg); err != nil {
		fmt.Printf("Error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n✓ Configured for AgentForms managed relay\n")
	fmt.Printf("  Sending via: %s\n", cfg.Host)
	fmt.Printf("  From: %s\n", cfg.From)
	fmt.Println("\nTest it: relay send --to you@example.com --body 'test'")
}

func setupCustom() {
	fmt.Println("\n--- Custom SMTP ---")
	fmt.Println()

	// Prompt for settings
	var host = "mail.example.com"
	var port = 587
	var fromAddr = ""
	var senderName = ""
	var user = ""
	var pass = ""

	fmt.Printf("SMTP host [%s]: ", host)
	readLine(&host)

	if host == "" {
		host = "mail.example.com"
	}

	fmt.Printf("SMTP port [%d]: ", port)
	readInt(&port)

	fmt.Print("From address: ")
	readLine(&fromAddr)
	if fromAddr == "" {
		fmt.Println("Error: from address is required")
		os.Exit(1)
	}

	fmt.Printf("Your name (displayed as sender, leave blank for none): ")
	readLine(&senderName)

	fmt.Print("SMTP username: ")
	readLine(&user)
	if user == "" {
		fmt.Println("Error: username is required")
		os.Exit(1)
	}

	fmt.Print("SMTP password: ")
	readLine(&pass)
	if pass == "" {
		fmt.Println("Error: password is required")
		os.Exit(1)
	}

	cfg := &Config{
		Host:       host,
		Port:       port,
		From:       fromAddr,
		SenderName: strings.TrimSpace(senderName),
		User:       user,
		Password:   pass,
		AuthType:   "PLAIN",
	}

	if err := saveConfig(cfg); err != nil {
		fmt.Printf("Error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n✓ Custom SMTP configured\n")
	fmt.Printf("  Host: %s:%d\n", cfg.Host, cfg.Port)
	fmt.Printf("  From: %s\n", cfg.From)
	fmt.Println("\nTest it: relay send --to you@example.com --body 'test'")
}

var (
	host     = "mail.agentforms.io"
	port     = 587
	fromAddr = "you@agentforms.io"
	user     string
	pass     string
)

func readLine(val *string) {
	fmt.Scanln(val)
	if *val == "" {
		// Keep default
	}
}

func readInt(val *int) {
	var s string
	fmt.Scanln(&s)
	if s == "" {
		return // Keep default
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		fmt.Printf("Invalid port: %v\n", err)
		os.Exit(1)
	}
	*val = v
}

func cmdSend(args []string) {
	// Parse args
	var toAddrs []string
	var bccAddrs []string
	var fromNameOverride string
	subject := "File from relay"
	body := ""
	htmlBody := ""
	var files []string
	var maxSize int64 // 0 = unlimited
	var fromDomain string   // Phase 1
	var relayHostOverride string // Phase 1
	var webhookURL string  // Phase 2
	var templateName string // Phase 3
	var templateVars map[string]string // Phase 3

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--to", "-t":
			i++
			if i < len(args) {
				toAddrs = strings.Split(args[i], ",")
				for j, t := range toAddrs {
					toAddrs[j] = strings.TrimSpace(t)
				}
			}
		case "--bcc", "-c":
			i++
			if i < len(args) {
				bccAddrs = strings.Split(args[i], ",")
				for j, t := range bccAddrs {
					bccAddrs[j] = strings.TrimSpace(t)
				}
			}
		case "--from-name", "-F":
			i++
			if i < len(args) {
				fromNameOverride = args[i]
			}
		case "--subject", "-s":
			i++
			if i < len(args) {
				subject = args[i]
			}
		case "--body", "-b":
			i++
			if i < len(args) {
				body = args[i]
			}
		case "--html-body", "-B":
			i++
			if i < len(args) {
				htmlBody = args[i]
			}
		case "--max-size":
			i++
			if i < len(args) {
				maxSize = parseSize(args[i])
			}
		case "--file", "-f":
			i++
			if i < len(args) {
				files = append(files, args[i])
			}
		// Phase 1: Custom from domain
		case "--from-domain":
			i++
			if i < len(args) {
				fromDomain = args[i]
			}
		case "--relay-host":
			i++
			if i < len(args) {
				relayHostOverride = args[i]
			}
		// Phase 2: Webhook
		case "--webhook":
			i++
			if i < len(args) {
				webhookURL = args[i]
			}
		// Phase 3: Templates
		case "--template":
			i++
			if i < len(args) {
				templateName = args[i]
			}
		case "--vars":
			i++
			if i < len(args) && templateVars == nil {
				templateVars = make(map[string]string)
			}
			if i < len(args) && templateVars != nil {
				parts := strings.SplitN(args[i], "=", 2)
				if len(parts) == 2 {
					templateVars[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
				}
			}
		default:
			fmt.Printf("Unknown flag: %s\n", args[i])
			os.Exit(1)
		}
		i++
	}

	if len(toAddrs) == 0 {
		fmt.Println("Error: --to is required")
		fmt.Println("Usage: relay send --to user@example.com [options]")
		os.Exit(1)
	}

	// Load config
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("Config error: %v\n", err)
		os.Exit(1)
	}

	// Rate limiting check (managed relay mode)
	if err := getRateLimiter(cfg).Allow(rateLimitKey(cfg)); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// Apply default attachment size limit from config or use global default
	if maxSize == 0 {
		if cfg.MaxAttachmentSize > 0 {
			maxSize = cfg.MaxAttachmentSize
		} else {
			maxSize = defaultMaxAttachment
		}
	}

	// Phase 5: Tier gate for from-domain (paid only)
	if fromDomain != "" && !isPaidTier(cfg) {
		fmt.Println("Error: --from-domain requires a paid tier. Link a paid account with 'relay link'.")
		os.Exit(1)
	}

	// Phase 5: Tier gate for relay-host (paid only)
	if relayHostOverride != "" && !isPaidTier(cfg) {
		fmt.Println("Error: --relay-host requires a paid tier.")
		os.Exit(1)
	}

	// Phase 5: Tier gate for webhooks (paid only)
	if webhookURL != "" && !isPaidTier(cfg) {
		fmt.Println("Error: --webhook requires a paid tier.")
		os.Exit(1)
	}

	// Phase 3: Template processing (paid only)
	if templateName != "" {
		if !isPaidTier(cfg) {
			fmt.Println("Error: --template requires a paid tier.")
			os.Exit(1)
		}
		templates, err := loadTemplates()
		if err != nil {
			fmt.Printf("Error loading templates: %v\n", err)
			os.Exit(1)
		}
		var found *EmailTemplate
		for _, t := range templates {
			if t.Name == templateName {
				found = &t
				break
			}
		}
		if found == nil {
			fmt.Printf("Template '%s' not found. Use 'relay templates list' to see available templates.\n", templateName)
			os.Exit(1)
		}

		// Render template
		if templateVars == nil {
			templateVars = make(map[string]string)
		}
		renderedSubject, err := renderTemplate(found.Subject, templateVars)
		if err != nil {
			fmt.Printf("Template error: %v\n", err)
			os.Exit(1)
		}
		subject = renderedSubject

		renderedBody, err := renderTemplate(found.Body, templateVars)
		if err != nil {
			fmt.Printf("Template error: %v\n", err)
			os.Exit(1)
		}
		if body == "" {
			body = renderedBody
		}

		if found.HTML != "" {
			renderedHTML, err := renderTemplate(found.HTML, templateVars)
			if err != nil {
				fmt.Printf("Template error: %v\n", err)
				os.Exit(1)
			}
			if htmlBody == "" {
				htmlBody = renderedHTML
			}
		}
	}

	// Apply sender name override if provided
	if fromNameOverride != "" {
		cfg.SenderName = fromNameOverride
	}

	// Phase 1: Override relay host if specified
	if relayHostOverride != "" {
		cfg.Host = relayHostOverride
	}

	// Phase 4: Generate tracking token
	trackingToken := generateTrackingToken()

	// Phase 4: Inject tracking pixel into HTML body (paid only)
	if htmlBody != "" && isPaidTier(cfg) {
		htmlBody = injectOpenPixel(htmlBody, trackingToken)
	}

	// Expand glob patterns
	var expandedFiles []string
	for _, f := range files {
		// Expand ~
		f = filepath.Clean(os.Expand(f, func(s string) string {
			if s == "~" {
				return homeDir()
			}
			return s
		}))
		matches, err := filepath.Glob(f)
		if err != nil {
			fmt.Printf("Error globbing %s: %v\n", f, err)
			continue
		}
		if len(matches) == 0 {
			fmt.Printf("No files matched: %s\n", f)
		} else {
			expandedFiles = append(expandedFiles, matches...)
		}
	}

	// Validate attachment sizes and total message size
	totalSize := int64(len(body) + len(htmlBody))
	for _, f := range expandedFiles {
		info, err := os.Stat(f)
		if err != nil {
			fmt.Printf("Error stating file %s: %v\n", f, err)
			os.Exit(1)
		}
		// Check per-file size limit
		if maxSize > 0 && info.Size() > maxSize {
			fmt.Printf("Error: file %s (%s) exceeds max attachment size %s\n",
				filepath.Base(f), formatBytes(info.Size()), formatBytes(maxSize))
			os.Exit(1)
		}
		totalSize += info.Size()
	}
	// Check total message size limit
	if totalSize > defaultMaxMessageSize {
		fmt.Printf("Error: total message size %s exceeds maximum %s\n",
			formatBytes(totalSize), formatBytes(defaultMaxMessageSize))
		os.Exit(1)
	}

	// Apply sender name override if provided
	if fromNameOverride != "" {
		cfg.SenderName = fromNameOverride
	}

	// Build params
	resolvedFrom := resolveFromAddress(cfg, fromDomain)
	params := &sendParams{
		to:            toAddrs,
		bcc:           bccAddrs,
		subject:       subject,
		body:          body,
		htmlBody:      htmlBody,
		files:         expandedFiles,
		maxSize:       maxSize,
		fromAddress:   resolvedFrom,   // Phase 1
		webhookURL:    webhookURL,     // Phase 2
		trackingToken: trackingToken,  // Phase 4
	}

	// Append relay footer for free tier
	if shouldShowFooter(cfg) {
		params.body += relayFooter
		if params.htmlBody != "" {
			params.htmlBody += fmt.Sprintf(`<p style="margin-top:24px;color:#999;font-size:12px;">-- Sent via AgentRelay | <a href="%s">More at agentforms.io</a></p>`, agentFormsURL)
		}
	}

	// Send email
	entry := SentEntry{
		To:            toAddrs,
		Subject:       subject,
		Body:          params.body,
		Files:         expandedFiles,
		Timestamp:     time.Now().Format(time.RFC3339),
		WebhookURL:    webhookURL,
		TrackingToken: trackingToken,
	}

	if err := sendMail(cfg, params); err != nil {
		entry.Status = "failed"
		entry.Error = err.Error()
		appendSentLog(entry)
		fmt.Printf("Error sending: %v\n", err)
		os.Exit(1)
	}

	entry.Status = "sent"

	// Phase 2: Send webhook callback after successful send
	if webhookURL != "" {
		wpayload := webhookPayload{
			Event:     "email.sent",
			Token:     trackingToken,
			To:        toAddrs,
			Subject:   subject,
			Timestamp: entry.Timestamp,
			Status:    "sent",
		}
		status := sendWebhook(webhookURL, wpayload)
		entry.WebhookStatus = status
		entry.WebhookAttempts = 3
	}

	appendSentLog(entry)

	// Print summary
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "=== Sent ===")
	fmt.Fprintf(w, "To:\t%s\n", strings.Join(toAddrs, ", "))
	if len(bccAddrs) > 0 {
		fmt.Fprintf(w, "Bcc:\t%s\n", strings.Join(bccAddrs, ", "))
	}
	fmt.Fprintf(w, "Subject:\t%s\n", subject)
	if fromDomain != "" {
		fmt.Fprintf(w, "From:\t%s\n", resolvedFrom)
	}
	if len(expandedFiles) > 0 {
		fmt.Fprintf(w, "Files (%d):\t\n", len(expandedFiles))
		for _, f := range expandedFiles {
			sz := fileSize(f)
			fmt.Fprintf(w, "  %s\t%s\n", filepath.Base(f), sz)
		}
	}
	if htmlBody != "" {
		fmt.Fprintf(w, "HTML:\tyes\n")
	}
	if templateName != "" {
		fmt.Fprintf(w, "Template:\t%s\n", templateName)
	}
	if trackingToken != "" {
		fmt.Fprintf(w, "Tracking:\t%s\n", trackingToken)
	}
	if webhookURL != "" {
		fmt.Fprintf(w, "Webhook:\t%s (%s)\n", webhookURL, entry.WebhookStatus)
	}
	fmt.Fprintf(w, "When:\t%s\n", entry.Timestamp)
	w.Flush()
}

func parseSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	// Try plain bytes first
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}

	// Parse with unit suffix
	lower := strings.ToLower(s)
	var unit int64 = 1
	switch {
	case strings.HasSuffix(lower, "g"):
		unit = 1024 * 1024 * 1024
	case strings.HasSuffix(lower, "m"):
		unit = 1024 * 1024
	case strings.HasSuffix(lower, "k"):
		unit = 1024
	default:
		return 0
	}

	numStr := strings.TrimSuffix(lower, strings.ToLower(s[len(s)-1:]))
	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0
	}
	return int64(n * float64(unit))
}

type sendParams struct {
	to            []string
	bcc           []string
	subject       string
	body          string
	htmlBody      string
	files         []string
	maxSize       int64 // 0 means unlimited
	fromAddress   string // Phase 1: resolved from address
	webhookURL    string // Phase 2: webhook callback URL
	trackingToken string // Phase 4: tracking token
}

func sendMail(cfg *Config, params *sendParams) error {
	auth := smtp.PlainAuth("", cfg.User, cfg.Password, cfg.Host)

	// Build multipart message
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// Phase 1: Use resolved from address
	fromAddr := params.fromAddress
	if fromAddr == "" {
		fromAddr = cfg.From
	}

	// Headers
	if cfg.SenderName != "" {
		fmt.Fprintf(&buf, "From: %s <%s>\r\n", cfg.SenderName, fromAddr)
	} else {
		fmt.Fprintf(&buf, "From: %s\r\n", fromAddr)
	}
	fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(params.to, ", "))
	// BCC recipients are NOT written to headers (standard SMTP BCC —
	// they get Rcpt() calls but stay invisible in the message)
	fmt.Fprintf(&buf, "Subject: %s\r\n", params.subject)
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")

	hasAttachments := len(params.files) > 0
	hasHTML := params.htmlBody != ""
	hasText := params.body != ""

	if hasAttachments {
		fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=%s\r\n\r\n", w.Boundary())
	} else {
		// No attachments — simpler MIME
		if hasHTML && hasText {
			altBoundary := w.Boundary()
			// Replace the boundary with a nested structure
			_ = altBoundary // handled below
		} else {
			fmt.Fprintf(&buf, "Content-Type: text/plain; charset=utf-8\r\n\r\n")
			buf.WriteString(params.body)
			w.Close()
			return doSMTP(cfg, params, &buf, auth)
		}
	}

	// Handle body content
	if hasHTML && hasText {
		// multipart/alternative for text + html
		altWriter := multipart.NewWriter(&buf)
		fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%s\r\n\r\n", altWriter.Boundary())

		// Text part
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		part, err := altWriter.CreatePart(hdr)
		if err != nil {
			return err
		}
		part.Write([]byte(params.body))

		// HTML part
		hdrHTML := textproto.MIMEHeader{}
		hdrHTML.Set("Content-Type", "text/html; charset=utf-8")
		partHTML, err := altWriter.CreatePart(hdrHTML)
		if err != nil {
			return err
		}
		partHTML.Write([]byte(params.htmlBody))
		altWriter.Close()
	} else if hasHTML {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Type", "text/html; charset=utf-8")
		part, err := w.CreatePart(hdr)
		if err != nil {
			return err
		}
		part.Write([]byte(params.htmlBody))
	} else if hasText {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		part, err := w.CreatePart(hdr)
		if err != nil {
			return err
		}
		part.Write([]byte(params.body))
	}

	// Attachments
	for _, f := range params.files {
		info, err := os.Stat(f)
		if err != nil {
			return fmt.Errorf("stat %s: %w", f, err)
		}

		// Check size limit
		if params.maxSize > 0 && info.Size() > params.maxSize {
			return fmt.Errorf("file %s (%s) exceeds max size %s",
				filepath.Base(f), formatBytes(info.Size()), formatBytes(params.maxSize))
		}

		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("reading %s: %w", f, err)
		}

		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Type", "application/octet-stream")
		hdr.Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(f)))
		hdr.Set("Content-Transfer-Encoding", "base64")

		part, err := w.CreatePart(hdr)
		if err != nil {
			return err
		}

		// Base64 encode in chunks
		encoder := base64.NewEncoder(base64.StdEncoding, part)
		chunk := make([]byte, 72*3/4) // ~54 bytes per line
		for len(data) > 0 {
			n := len(data)
			if n > len(chunk) {
				n = len(chunk)
			}
			encoder.Write(data[:n])
			data = data[n:]
		}
		encoder.Close()
	}

	w.Close()

	return doSMTP(cfg, params, &buf, auth)
}

func doSMTP(cfg *Config, params *sendParams, buf *bytes.Buffer, auth smtp.Auth) error {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	// Build SMTP client
	var conn smtpClient
	var err error

	if cfg.Port == 465 {
		tlsConfig := &tls.Config{
			ServerName: cfg.Host,
		}
		netConn, err := tls.Dial("tcp", addr, tlsConfig)
		if err != nil {
			return fmt.Errorf("TLS connect to %s: %w", addr, err)
		}
		sc, err := smtp.NewClient(netConn, cfg.Host)
		if err != nil {
			netConn.Close()
			return fmt.Errorf("SMTP client: %w", err)
		}
		conn = sc
	} else {
		// STARTTLS — plain connect then upgrade
		netConn, err := net.Dial("tcp", addr)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", addr, err)
		}
		sc, err := smtp.NewClient(netConn, cfg.Host)
		if err != nil {
			netConn.Close()
			return fmt.Errorf("SMTP client: %w", err)
		}
		// Upgrade to TLS via STARTTLS
		if ok, _ := sc.Extension("STARTTLS"); ok {
			tlsConfig := &tls.Config{
				ServerName: cfg.Host,
			}
			if err := sc.StartTLS(tlsConfig); err != nil {
				return fmt.Errorf("STARTTLS: %w", err)
			}
		}
		conn = sc
	}
	defer conn.Close()

	if err := conn.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	// Phase 1: Use resolved from address
	fromAddr := params.fromAddress
	if fromAddr == "" {
		fromAddr = cfg.From
	}

	if err := conn.Mail(fromAddr); err != nil {
		return fmt.Errorf("from: %w", err)
	}

	// All recipients (To + BCC)
	allRecipients := append(params.to, params.bcc...)
	for _, r := range allRecipients {
		if err := conn.Rcpt(r); err != nil {
			return fmt.Errorf("rcpt %s: %w", r, err)
		}
	}

	wc, err := conn.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}

	_, err = io.Copy(wc, buf)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	return wc.Close()
}

func fileSize(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "?"
	}
	return formatBytes(info.Size())
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGT"[exp])
}

func cmdSent(args []string) {
	limit := 10
	var showOpens, showBounces bool
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-n", "--n":
			i++
			if i < len(args) {
				n, err := strconv.Atoi(args[i])
				if err == nil && n > 0 {
					limit = n
				}
			}
		case "--opens":
			showOpens = true
		case "--bounces":
			showBounces = true
		}
		i++
	}

	entries, err := loadSentLog()
	if err != nil {
		fmt.Printf("Error loading sent log: %v\n", err)
		os.Exit(1)
	}

	if len(entries) == 0 {
		fmt.Println("No sent emails yet.")
		return
	}

	// Show last N entries
	start := len(entries) - limit
	if start < 0 {
		start = 0
	}
	entries = entries[start:]

	fmt.Printf("=== Sent History (last %d) ===\n\n", len(entries))
	for i, e := range entries {
		fmt.Printf("#%d  [%s] %s -> %s\n", i+1, e.Timestamp, e.Subject, strings.Join(e.To, ", "))
		if len(e.Files) > 0 {
			fmt.Printf("     Files: %d attached\n", len(e.Files))
		}
		if e.Status == "failed" {
			fmt.Printf("     ❌ %s\n", e.Error)
		} else {
			fmt.Printf("     ✓ Sent\n")
		}
		// Phase 4: Show tracking info
		if showOpens && e.TrackingToken != "" {
			fmt.Printf("     Opens: %d", e.OpenCount)
			if e.LastOpened != "" {
				fmt.Printf(" (last: %s)", e.LastOpened)
			}
			fmt.Printf("\n")
		}
		if showBounces && e.TrackingToken != "" {
			if e.BounceCount > 0 {
				fmt.Printf("     Bounces: %d (last: %s)\n", e.BounceCount, e.BouncedAt)
			} else {
				fmt.Printf("     Bounces: 0\n")
			}
		}
		// Phase 2: Show webhook status
		if e.WebhookURL != "" {
			fmt.Printf("     Webhook: %s\n", e.WebhookStatus)
		}
		fmt.Println()
	}
}

func cmdUseDefaults() {
	// Remove custom config to fall back to defaults
	path := configPath()
	if err := os.Remove(path); err == nil || os.IsNotExist(err) {
		fmt.Println("✓ Reset to AgentForms managed relay")
		fmt.Printf("  Sending via: %s\n", defaultConfig.Host)
		fmt.Printf("  From: %s\n", defaultConfig.From)
		fmt.Println("\n  You're all set — just run: relay send --to user@example.com --file report.pdf")
	} else {
		fmt.Printf("Error removing config: %v\n", err)
		os.Exit(1)
	}
}

func cmdMetrics() {
	entries, err := loadSentLog()
	if err != nil {
		fmt.Printf("Error loading sent log: %v\n", err)
		os.Exit(1)
	}

	totalSends := 0
	totalOpens := 0
	uniqueOpens := 0
	totalBounces := 0

	for _, e := range entries {
		if e.Status == "sent" {
			totalSends++
		}
		totalOpens += e.OpenCount
		uniqueOpens += len(e.UniqueOpens)
		totalBounces += e.BounceCount
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "=== Delivery Metrics ===")
	fmt.Fprintf(w, "Total sent:\t%d\n", totalSends)
	fmt.Fprintf(w, "Total opens:\t%d\n", totalOpens)
	fmt.Fprintf(w, "Unique opens:\t%d\n", uniqueOpens)
	fmt.Fprintf(w, "Total bounces:\t%d\n", totalBounces)
	if totalSends > 0 {
		openRate := float64(uniqueOpens) / float64(totalSends) * 100
		bounceRate := float64(totalBounces) / float64(totalSends) * 100
		fmt.Fprintf(w, "Open rate:\t%.1f%%\n", openRate)
		fmt.Fprintf(w, "Bounce rate:\t%.1f%%\n", bounceRate)
	}
	w.Flush()
}

func cmdServe(args []string) {
	port := 5090
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--port":
			i++
			if i < len(args) {
				p, err := strconv.Atoi(args[i])
				if err == nil && p > 0 {
					port = p
				}
			}
		}
		i++
	}

	// Templates
	tmpl := htmltmpl.Must(htmltmpl.New("").Parse(`
<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Agent Relay</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: #0a0a0a; color: #e0e0e0; }
        .container { max-width: 900px; margin: 0 auto; padding: 2rem; }
        h1 { font-size: 1.5rem; margin-bottom: 2rem; color: #fff; }
        .tabs { display: flex; gap: 0.5rem; margin-bottom: 2rem; border-bottom: 1px solid #333; }
        .tab { padding: 0.75rem 1.5rem; cursor: pointer; border-bottom: 2px solid transparent; color: #888; }
        .tab.active { color: #fff; border-bottom-color: #3b82f6; }
        .tab-content { display: none; }
        .tab-content.active { display: block; }
        .form-group { margin-bottom: 1rem; }
        label { display: block; margin-bottom: 0.5rem; color: #888; font-size: 0.875rem; }
        input, textarea { width: 100%; padding: 0.75rem; background: #1a1a1a; border: 1px solid #333; color: #e0e0e0; border-radius: 6px; font-size: 0.875rem; }
        input:focus, textarea:focus { outline: none; border-color: #3b82f6; }
        textarea { min-height: 120px; resize: vertical; }
        button { padding: 0.75rem 1.5rem; background: #3b82f6; color: #fff; border: none; border-radius: 6px; cursor: pointer; font-size: 0.875rem; }
        button:hover { background: #2563eb; }
        .file-list { margin: 1rem 0; }
        .file-item { display: flex; align-items: center; gap: 0.5rem; padding: 0.5rem; background: #1a1a1a; border-radius: 4px; margin-bottom: 0.25rem; }
        .file-item .remove { margin-left: auto; color: #ef4444; cursor: pointer; }
        .drop-zone { border: 2px dashed #333; border-radius: 8px; padding: 2rem; text-align: center; color: #666; cursor: pointer; transition: border-color 0.2s; }
        .drop-zone:hover, .drop-zone.dragover { border-color: #3b82f6; color: #3b82f6; }
        .drop-zone input { display: none; }
        .sent-item { padding: 1rem; background: #1a1a1a; border-radius: 6px; margin-bottom: 0.5rem; }
        .sent-item .to { color: #3b82f6; }
        .sent-item .time { color: #666; font-size: 0.75rem; }
        .status-ok { color: #22c55e; }
        .status-fail { color: #ef4444; }
        .toast { position: fixed; bottom: 2rem; right: 2rem; padding: 1rem 1.5rem; border-radius: 6px; background: #22c55e; color: #fff; display: none; }
        .toast.error { background: #ef4444; }
    </style>
</head>
<body>
<div class="container">
    <h1>📬 Agent Relay</h1>
    <div class="tabs">
        <div class="tab active" onclick="showTab('compose')">Compose</div>
        <div class="tab" onclick="showTab('sent')">Sent</div>
    </div>
    <div id="compose" class="tab-content active">
        <form id="sendForm" onsubmit="sendMail(event)">
            <div class="form-group">
                <label>To (comma-separated)</label>
                <input type="email" id="to" required placeholder="user@example.com">
            </div>
            <div class="form-group">
                <label>Subject</label>
                <input type="text" id="subject" placeholder="File from relay">
            </div>
            <div class="form-group">
                <label>Body (plain text)</label>
                <textarea id="body" placeholder="Optional message..."></textarea>
            </div>
            <div class="form-group">
                <label>HTML Body (optional, for rich formatting)</label>
                <textarea id="html_body" placeholder="<h1>Hello</h1><p>Rich HTML email...</p>"></textarea>
            </div>
            <div class="form-group">
                <label>From Domain (optional, paid)</label>
                <input type="text" id="from_domain" placeholder="mycompany.com">
            </div>
            <div class="form-group">
                <label>Relay Host (optional, paid)</label>
                <input type="text" id="relay_host" placeholder="smtp.mycompany.com">
            </div>
            <div class="form-group">
                <label>Webhook URL (optional, paid)</label>
                <input type="text" id="webhook_url" placeholder="https://hooks.example.com/delivered">
            </div>
            <div class="form-group">
                <label>Attachments</label>
                <div class="file-list" id="fileList"></div>
                <div class="drop-zone" id="dropZone" onclick="document.getElementById('fileInput').click()">
                    <input type="file" id="fileInput" multiple onchange="addFiles(event)">
                    <div>📎 Drop files here or click to browse</div>
                </div>
            </div>
            <button type="submit">Send</button>
        </form>
    </div>
    <div id="sent" class="tab-content">
        <div id="sentList"></div>
    </div>
</div>
<div class="toast" id="toast"></div>
<script>
    function showTab(id) {
        document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
        document.querySelectorAll('.tab-content').forEach(t => t.classList.remove('active'));
        document.getElementById(id).classList.add('active');
        event.target.classList.add('active');
        if (id === 'sent') loadSent();
    }
    let files = [];
    function addFiles(e) {
        for (const f of e.target.files) {
            files.push(f);
        }
        renderFiles();
        e.target.value = '';
    }
    function renderFiles() {
        const list = document.getElementById('fileList');
        const dz = document.getElementById('dropZone');
        list.innerHTML = files.map((f, i) => {
            const sz = f.size > 1048576 ? (f.size/1048576).toFixed(1)+'MB' : (f.size/1024).toFixed(0)+'KB';
            return '<div class="file-item">📎 '+f.name+' <span style="color:#666">'+sz+'</span><span class="remove" onclick="removeFile('+i+')">×</span></div>';
        }).join('');
        dz.style.display = files.length > 0 ? 'none' : 'block';
    }
    function removeFile(i) { files.splice(i, 1); renderFiles(); }

    // Drag and drop
    const dropZone = document.getElementById('dropZone');
    dropZone.addEventListener('dragover', (e) => { e.preventDefault(); dropZone.classList.add('dragover'); });
    dropZone.addEventListener('dragleave', () => { dropZone.classList.remove('dragover'); });
    dropZone.addEventListener('drop', (e) => {
        e.preventDefault();
        dropZone.classList.remove('dragover');
        for (const f of e.dataTransfer.files) {
            files.push(f);
        }
        renderFiles();
    });
    async function sendMail(e) {
        e.preventDefault();
        const formData = new FormData();
        formData.append('to', document.getElementById('to').value);
        formData.append('subject', document.getElementById('subject').value || 'File from relay');
        formData.append('body', document.getElementById('body').value);
        formData.append('html_body', document.getElementById('html_body').value);
        formData.append('from_domain', document.getElementById('from_domain').value);
        formData.append('relay_host', document.getElementById('relay_host').value);
        formData.append('webhook_url', document.getElementById('webhook_url').value);
        for (const f of files) {
            formData.append('attachments', f, f.name);
        }
        const btn = e.target.querySelector('button');
        btn.disabled = true;
        btn.textContent = 'Sending...';
        try {
            const resp = await fetch('/api/send', { method: 'POST', body: formData });
            const data = await resp.json();
            if (data.error) {
                showToast(data.error, true);
            } else {
                showToast('✓ Sent to ' + data.to);
                document.getElementById('sendForm').reset();
                files = [];
                renderFiles();
            }
        } catch(err) {
            showToast('Network error: ' + err.message, true);
        }
        btn.disabled = false;
        btn.textContent = 'Send';
    }
    async function loadSent() {
        const resp = await fetch('/api/sent');
        const data = await resp.json();
        const list = document.getElementById('sentList');
        if (data.length === 0) { list.innerHTML = '<p style="color:#666">No sent emails yet.</p>'; return; }
        list.innerHTML = data.map(e => '<div class="sent-item"><div class="to">'+e.to+'</div><div>'+e.subject+'</div><div class="time">'+e.timestamp+' <span class="'+(e.status==='sent'?'status-ok':'status-fail')+'">'+e.status+'</span></div></div>').join('');
    }
    function showToast(msg, isError) {
        const t = document.getElementById('toast');
        t.textContent = msg;
        t.className = 'toast' + (isError ? ' error' : '');
        t.style.display = 'block';
        setTimeout(() => t.style.display = 'none', 3000);
    }
    loadSent();
</script>
</body>
</html>`))

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		tmpl.Execute(w, nil)
	})

	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		// CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte(`{"error":"POST only"}`))
			return
		}
		_ = r.ParseMultipartForm(32 << 20) // 32MB max
		to := r.FormValue("to")
		subject := r.FormValue("subject")
		body := r.FormValue("body")
		htmlBody := r.FormValue("html_body")
		fromDomain := r.FormValue("from_domain")    // Phase 1
		relayHost := r.FormValue("relay_host")      // Phase 1
		webhookURL := r.FormValue("webhook_url")    // Phase 2

		cfg, err := loadConfig()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		// Rate limiting check
		if err := getRateLimiter(cfg).Allow(rateLimitKey(cfg)); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		toAddrs := strings.Split(to, ",")
		for i, t := range toAddrs {
			toAddrs[i] = strings.TrimSpace(t)
		}

		// Phase 5: Tier gates for web form
		if fromDomain != "" && !isPaidTier(cfg) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "--from-domain requires a paid tier"})
			return
		}
		if webhookURL != "" && !isPaidTier(cfg) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "--webhook requires a paid tier"})
			return
		}

		// Override relay host if specified
		if relayHost != "" && isPaidTier(cfg) {
			cfg.Host = relayHost
		}

		// Phase 4: Generate tracking token
		trackingToken := generateTrackingToken()

		// Phase 4: Inject tracking pixel into HTML body (paid only)
		if htmlBody != "" && isPaidTier(cfg) {
			htmlBody = injectOpenPixel(htmlBody, trackingToken)
		}

		// Phase 4: Inject tracking pixel into body (paid only, if body looks like HTML)
		if htmlBody == "" && body != "" && isPaidTier(cfg) {
			// If body contains HTML tags, inject pixel
			if strings.Contains(body, "<") {
				body = injectOpenPixel(body, trackingToken)
			}
		}

		// Resolve from address
		resolvedFrom := resolveFromAddress(cfg, fromDomain)

		// Handle uploaded file attachments
		var uploadedFiles []string
		var cleanupFiles []string
		var webMaxSize int64 = defaultMaxAttachment
		if cfg.MaxAttachmentSize > 0 {
			webMaxSize = cfg.MaxAttachmentSize
		}
		var webTotalSize int64 = int64(len(body) + len(htmlBody))
		if r.MultipartForm != nil && r.MultipartForm.File != nil {
			multipartFiles := r.MultipartForm.File["attachments"]
			for _, mf := range multipartFiles {
				// Check size limit before saving
				if webMaxSize > 0 && mf.Size > webMaxSize {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("file %s (%s) exceeds max attachment size %s",
						mf.Filename, formatBytes(mf.Size), formatBytes(webMaxSize))})
					return
				}
				webTotalSize += mf.Size
			}
			// Check total message size
			if webTotalSize > defaultMaxMessageSize {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("total message size %s exceeds maximum %s",
					formatBytes(webTotalSize), formatBytes(defaultMaxMessageSize))})
				return
			}
			for _, mf := range multipartFiles {
				src, err := mf.Open()
				if err != nil {
					continue
				}
				tmp, err := os.CreateTemp("", "relay-*-"+filepath.Base(mf.Filename))
				if err != nil {
					src.Close()
					continue
				}
				_, err = io.Copy(tmp, src)
				src.Close()
				tmp.Close()
				if err != nil {
					os.Remove(tmp.Name())
					continue
				}
				uploadedFiles = append(uploadedFiles, tmp.Name())
				cleanupFiles = append(cleanupFiles, tmp.Name())
			}
		}

		// Apply tier-aware footer (same as CLI)
		showFooter := shouldShowFooter(cfg)
		if showFooter {
			body += relayFooter
		}

		entry := SentEntry{
			To:            toAddrs,
			Subject:       subject,
			Body:          body,
			Files:         uploadedFiles,
			Timestamp:     time.Now().Format(time.RFC3339),
			WebhookURL:    webhookURL,
			TrackingToken: trackingToken,
		}

		params := &sendParams{
			to:            toAddrs,
			subject:       subject,
			body:          body,
			htmlBody:      htmlBody,
			files:         uploadedFiles,
			fromAddress:   resolvedFrom,
			webhookURL:    webhookURL,
			trackingToken: trackingToken,
		}

		if err := sendMail(cfg, params); err != nil {
			entry.Status = "failed"
			entry.Error = err.Error()
			appendSentLog(entry)
			// Cleanup temp files on failure
			for _, f := range cleanupFiles {
				os.Remove(f)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		entry.Status = "sent"

		// Phase 2: Send webhook after successful send
		if webhookURL != "" && isPaidTier(cfg) {
			wpayload := webhookPayload{
				Event:     "email.sent",
				Token:     trackingToken,
				To:        toAddrs,
				Subject:   subject,
				Timestamp: entry.Timestamp,
				Status:    "sent",
			}
			status := sendWebhook(webhookURL, wpayload)
			entry.WebhookStatus = status
			entry.WebhookAttempts = 3
		}

		appendSentLog(entry)

		// Cleanup temp files on success
		for _, f := range cleanupFiles {
			os.Remove(f)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "to": strings.Join(toAddrs, ", ")})
	})

	mux.HandleFunc("/api/sent", func(w http.ResponseWriter, r *http.Request) {
		entries, err := loadSentLog()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// Return last 50
		if len(entries) > 50 {
			entries = entries[len(entries)-50:]
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
	})

	// Phase 4: Tracking endpoints
	mux.HandleFunc("/track/open/", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/track/open/")
		if token == "" {
			http.Error(w, "bad request", 400)
			return
		}
		recordOpen(token)
			// Return 1x1 transparent GIF
			w.Header().Set("Content-Type", "image/gif")
			w.Write([]byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x21, 0xf9, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44, 0x00, 0x3b})
	})

	mux.HandleFunc("/track/bounce/", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/track/bounce/")
		if token == "" {
			http.Error(w, "bad request", 400)
			return
		}
		recordBounce(token)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "recorded"})
	})

	// Phase 4: Metrics endpoint
	mux.HandleFunc("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		entries, err := loadSentLog()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		totalSends := 0
		totalOpens := 0
		uniqueOpens := 0
		totalBounces := 0

		for _, e := range entries {
			if e.Status == "sent" {
				totalSends++
			}
			totalOpens += e.OpenCount
			uniqueOpens += len(e.UniqueOpens)
			totalBounces += e.BounceCount
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_sends":   totalSends,
			"total_opens":   totalOpens,
			"unique_opens":  uniqueOpens,
			"total_bounces": totalBounces,
		})
	})

	fmt.Printf("📬 Agent Relay running on http://0.0.0.0:%d\n", port)
	fmt.Println("Press Ctrl+C to stop")

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}

	command := os.Args[1]
	args := os.Args[2:]

	switch command {
	case "setup":
		cmdSetup()
	case "use-defaults":
		cmdUseDefaults()
	case "send":
		cmdSend(args)
	case "link":
		cmdLink()
	case "unlink":
		cmdUnlink()
	case "sent", "log":
		cmdSent(args)
	case "serve":
		cmdServe(args)
	case "templates":
		cmdTemplates(args)
	case "metrics":
		cmdMetrics()
	case "help", "--help", "-h":
		printUsage()
	case "version":
		fmt.Println("agent-relay v0.3.1")
	default:
		fmt.Printf("Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}
