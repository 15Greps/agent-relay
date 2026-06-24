<div align="center">

# agent-relay

**A terminal-native file relay — send any file via email from the command line or browser.**

[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat)](LICENSE)
[![Version](https://img.shields.io/badge/version-0.2.0-blue.svg?style=flat)](CHANGELOG.md)
[![Security](https://img.shields.io/badge/Security-Policy-green?style=flat)](SECURITY.md)

</div>

## Overview

Agent Relay is a single-binary Go tool for sending files via SMTP from the terminal. It was designed for AI agents that need to deliver files to human email inboxes — but it works great for anyone who wants a dead-simple CLI email client.

No dependencies. No daemon. No configuration file larger than your address book.

```bash
relay send -t user@example.com -f report.pdf
```

## Features

| Feature | Description |
|---------|-------------|
| **CLI send** | Single/multiple recipients, multiple files, glob patterns |
| **Zero-config** | Ships with managed relay — just works out of the box |
| **Web UI** | Optional browser interface on `:5090` with compose and sent log |
| **Interactive setup** | `relay setup` walks you through SMTP configuration |
| **Sent log** | Persistent delivery history with status tracking |
| **Cross-platform** | Pre-built for Linux (amd64, arm64, armv7), macOS (Intel, Apple Silicon), Windows amd64 |
| **Zero dependencies** | Single compiled Go binary, ~8 MB, no runtime |
| **Managed or self-hosted** | Use AgentForms relay or bring your own SMTP |

## Quick Install

```bash
curl -sL https://relay.agentforms.io/install.sh | bash
```

Installs to `~/.local/bin/relay`. Override with `INSTALL_DIR=/custom/path`.

Requires `~/.local/bin` in your `$PATH` (standard on most Linux systems).

### Zero-Config Managed Relay

Just works out of the box — sends through AgentForms' mail server:

```bash
relay send -t customer@example.com -f invoice.pdf
```

No setup required. No account needed. Every binary ships with managed relay credentials.

### Custom SMTP

Want to use your own mail server?

```bash
relay setup
```

Or reset to managed relay anytime:

```bash
relay use-defaults
```

### Build from Source

```bash
git clone https://github.com/agentforms/agent-relay.git
cd agent-relay
make build
make install
```

### Manual Install

**Linux/macOS:**
```bash
curl -sL https://relay.agentforms.io/install.sh | bash
```

**Windows (PowerShell):**
```powershell
irm https://relay.agentforms.io/install.ps1 | iex
```

**Manual binary download:**
```bash
# Linux amd64
curl -sL https://relay.agentforms.io/relay-linux-amd64 -o relay
chmod +x relay
sudo mv relay /usr/local/bin/
```

## Usage

### CLI

```bash
# Send a file
relay send -t user@example.com -f ~/report.pdf

# Multiple recipients
relay send -t a@x.com,b@y.com -s "Invoice #1234" -f invoice.pdf

# Multiple files with glob
relay send -t customer@example.com -f ~/data/*.csv

# Custom body text
relay send -t boss@example.com -f report.pdf \
  -b "Monthly report attached. Please review."

# Check sent log
relay sent -n 10
```

#### Send flags

| Flag | Short | Description |
|------|-------|-------------|
| `--to` | `-t` | Recipient email(s), comma-separated |
| `--subject` | `-s` | Email subject line |
| `--body` | `-b` | Plain text body |
| `--file` | `-f` | File(s) to attach (repeatable, supports globs) |

### Web UI

```bash
relay serve                 # → http://localhost:5090
relay serve --port 8080     # custom port (optional)
```

Serves a web interface with compose, file picker, and sent log viewer. Binds to localhost by default — use `--listen 0.0.0.0` for remote access (recommended behind Tailscale or reverse proxy).

### Commands

| Command | Description |
|---------|-------------|
| `relay setup` | Interactive SMTP configuration wizard |
| `relay use-defaults` | Reset to AgentForms managed relay |
| `relay send` | Send an email with attachments |
| `relay sent` | View sent history |
| `relay serve` | Start the web UI server |
| `relay version` | Print version |
| `relay help` | Print usage |

## Configuration

### Interactive Setup

```bash
relay setup
```

Prompts for SMTP host, port, username, password, and sender address. Saves to `~/.config/agent-relay/config.json`.

### Manual Config

Create `~/.config/agent-relay/config.json`:

```json
{
  "host": "smtp.example.com",
  "port": 587,
  "user": "your-smtp-username",
  "password": "your-password",
  "from": "you@example.com",
  "auth_type": "PLAIN"
}
```

#### Config fields

| Field | Required | Description |
|-------|----------|-------------|
| `host` | Yes | SMTP server hostname |
| `port` | Yes | SMTP port (587 for STARTTLS, 465 for SSL) |
| `user` | Yes | SMTP authentication username |
| `password` | Yes | SMTP authentication password |
| `from` | Yes | Sender email address |
| `auth_type` | No | Auth mechanism: `PLAIN` (default) or `LOGIN` |

### Storage locations

| Path | Contents |
|------|----------|
| `~/.config/agent-relay/config.json` | SMTP credentials (mode 0600) |
| `~/.local/state/agent-relay/sent.json` | Sent email log |

## Architecture

```
┌─────────────┐     CLI/Web              ┌────────────────┐
│  You or     │ ── attach + send ──────> │  relay binary  │
│  AI Agent   │ <── status ───────────── │  (~8 MB)       │
└─────────────┘                          └──────┬─────────┘
                                                │ config.json
                                                ▼
                                         ┌────────────────┐
                                         │  SMTP Server   │
                                         │  (STARTTLS)    │
                                         └──────┬─────────┘
                                                │
                                                ▼
                                         ┌────────────────┐
                                         │  Recipient     │
                                         │  Inbox         │
                                         └────────────────┘
```

Single binary. No daemon. No external dependencies beyond SMTP.

## Security

- Config file stored with `0600` permissions (owner read/write only)
- STARTTLS encryption for SMTP connections
- No telemetry, analytics, or phone home
- No external dependencies — audit the entire codebase in one file

See [SECURITY.md](SECURITY.md) for responsible disclosure policy.

## Agent Integration

Agents call relay via shell command. Exit codes indicate success/failure:

```bash
relay send \
  --to recipient@example.com \
  --subject "Report" \
  --body "Please find attached" \
  --file /path/to/report.pdf
```

| Exit code | Meaning |
|-----------|---------|
| `0` | Sent successfully |
| `1` | Configuration error (missing/invalid config) |
| `2` | SMTP delivery failure |

## Requirements

- **OS:** Linux (amd64, arm64, armv7), macOS (amd64, arm64), Windows (amd64)
- **Dependencies:** None — single Go binary
- **SMTP server:** Any SMTP server supporting STARTTLS

## Troubleshooting

### "no config found"

This shouldn't happen in v0.2.0+ — the relay ships with managed defaults. If you see this, run:

```bash
relay use-defaults
```

Or configure your own SMTP:

```bash
relay setup
```

### "tls: first record does not look like a TLS handshake"

This shouldn't happen in v0.2.0+ — the relay auto-detects TLS mode based on port:
- Port 465 → Direct TLS
- Port 587 → STARTTLS

If you're still seeing this, double-check your port configuration.

### "auth: 535 Authentication failed"

Check `user` and `password` in `~/.config/agent-relay/config.json`. Try logging into your SMTP server manually to verify credentials.

### Permissions denied on sent log

The relay creates `~/.local/state/agent-relay/` automatically. If you have a restrictive umask, run:

```bash
mkdir -p ~/.local/state/agent-relay
chmod 700 ~/.local/state/agent-relay
```

## Roadmap

- [x] Direct TLS (port 465) support — auto-detected by port
- [x] HTML email body support — `--html-body` / `-B` flag
- [x] BCC recipient mode — `--bcc` / `-c` flag
- [x] Attachment size limit config — `--max-size` with human-readable suffixes
- [x] Web UI file upload — drag-and-drop zone, file list, size display
- [x] macOS / Windows builds — cross-compile via `make build-all`, install scripts for all platforms
- [x] systemd service template — `agent-relay.service`

## Contributing

We welcome contributions. Please read [CONTRIBUTING.md](CONTRIBUTING.md) before opening issues or PRs.

By participating, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

[MIT](LICENSE) — Copyright (c) 2026 AgentForms

---

<div align="center">

Built by [AgentForms](https://agentforms.io) — Forms that turn into documents.

</div>
