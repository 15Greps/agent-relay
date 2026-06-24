# Changelog

All notable changes to Agent Relay will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/), and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.3.1] - 2026-06-24

### Added
- **Rate Limiting** — Per-token/per-API-key rate limiting for managed relay mode
  - In-memory sliding window (1-hour window, auto-pruning every 30 min)
  - Config field: `MaxSendsPerHour` (0=unlimited, default: 30/hour free, 100/hour paid)
  - Rate limit check in both CLI send and web UI `/api/send`
- **HTML Email Body Support** — Full multipart/alternative email support
  - `--html-body` / `-B` CLI flag for HTML body in direct sends
  - Template HTML field (`EmailTemplate.HTML`) wired into message builder
  - HTML body field in web UI compose form
  - Proper `multipart/alternative` (text/plain + text/html) when both bodies provided
- **BCC Recipient Mode** — Standard SMTP BCC behavior
  - `--bcc` / `-c` flag accepts comma-separated emails
  - BCC recipients receive via `Rcpt()` but are NOT in message headers
- **Attachment Size Limit Config** — Configurable size limits
  - Config field: `MaxAttachmentSize` (bytes, 0=unlimited, default 25MB per file)
  - `--max-size` CLI flag (e.g. "10M", "500K", "1g")
  - Total message size validation (default 35MB for body + all attachments)
  - Size checks before reading files to avoid unnecessary I/O
- **Web UI file upload** — Full file upload support in web interface
  - Drag-and-drop zone for file attachments
  - Server-side size validation (per-file and total)
  - HTML body textarea in compose form
  - Multipart/form-data handling with temp file management

### Changed
- BCC recipients no longer appear in message headers (correct SMTP BCC behavior)
- Version bumped to 0.3.1
- Added `sync` import for rate limiter mutex support

## [0.3.0] - 2026-06-23

### Added
- **Custom From Domain** — `--from-domain` flag sends from your own domain (paid tier)
- **Callback Webhooks** — `--webhook` flag POSTs delivery receipt to callback URL
- Webhook 3-attempt exponential backoff (0s, 1s, 2s)
- Config-level webhook URL (`webhook_url` in config.json)
- Webhook URL and From Domain fields in web UI
- Webhook tracking in SentEntry (webhook_url, webhook_attempts, webhook_status)
- **Template Sends** — `--template <slug> --vars '{...}'` renders Go text/template
- `relay templates sync` — pull email templates from AgentForms API (paid tier)
- `relay templates list/show/add` — manage local templates
- Web UI template_slug and vars_json form fields
- **Delivery Tracking** — open pixel injection in HTML emails
- `/track/open/{token}` endpoint serves 1x1 transparent GIF and logs opens
- `/track/bounce/{token}` endpoint for bounce notifications
- Tracking token generation on every send (16-byte hex)
- `relay sent --opens` and `relay sent --bounces` CLI commands
- SentEntry fields: tracking_token, open_count, last_opened, bounce_count, bounced_at

### Changed
- `fromHeader()` now takes (fromAddr, senderName) for custom domain support
- `sendMail()` accepts pre-resolved from address and webhook URL in params

## [0.2.0] - 2026-06-22

### Added
- **Zero-config managed relay** — ships with AgentForms mail server credentials
- `relay use-defaults` command to reset to managed relay
- Build-time token injection via `-ldflags` (no secrets in source)
- `relay setup` offers managed vs custom SMTP choice

### Changed
- `loadConfig()` falls back to managed relay instead of erroring
- Version bumped to 0.2.0

## [0.1.0] - 2026-06-22

### Added
- CLI commands: `setup`, `send`, `sent`, `serve`, `version`, `help`
- SMTP email delivery with STARTTLS encryption
- Multiple recipients (comma-separated)
- Multiple file attachments with glob pattern support
- Persistent sent log with status tracking
- Web UI on port 5090 with compose and sent log viewer
- Interactive SMTP configuration wizard
- Cross-platform builds: Linux amd64, arm64, armv7
- One-line installer with architecture detection

### Notes
- Initial open-source release
- Config stored at `~/.config/agent-relay/config.json` (mode 0600)
- Sent log at `~/.local/state/agent-relay/sent.json`
