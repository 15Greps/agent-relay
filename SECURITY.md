# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| 0.1.x   | ✅ Active |

## Reporting a Vulnerability

**Do not open a public issue for security vulnerabilities.**

Report privately via:

1. **Email:** security@agentforms.io (preferred)
2. **GitHub Security Advisory:** Use the "Report a vulnerability" tab on our [Security page](https://github.com/agentforms/agent-relay/security)

Include:
- Vulnerability type and severity
- Steps to reproduce
- Impact assessment (what data or systems are affected)
- Suggested fix (if you have one)

## Response Timeline

| Phase | Timeline |
|-------|----------|
| Acknowledgment | Within 48 hours |
| Initial assessment | Within 1 week |
| Fix or mitigation | Within 2 weeks (or sooner for critical) |
| Public disclosure | After fix is released |

## What to Expect

- You'll receive an acknowledgment within 48 hours
- We'll keep you updated on the investigation progress
- You'll be credited in the release notes (unless you prefer anonymity)
- We'll coordinate disclosure timing

## Scope

In scope:
- Credential exposure or mishandling
- SMTP authentication bypass
- Command injection via CLI arguments
- Buffer overflows or memory safety issues
- File path traversal in attachment handling

Out of scope:
- DoS via resource exhaustion (the relay is single-user by design)
- Social engineering
- Issues in third-party SMTP servers

## Security Design

Agent Relay is designed with these principles:

- **Minimal attack surface:** Single binary, no network listeners by default, no external dependencies
- **Secure defaults:** Config files stored with `0600` permissions, binds to localhost for web UI
- **No telemetry:** Zero data leaves the machine except through explicit send commands
- **Auditability:** Entire codebase in a single file — readable in under 10 minutes

## Known Limitations

- Port 465 (direct TLS) not yet supported — use port 587 with STARTTLS
- No attachment size limit enforcement — large files may be rejected by your SMTP server
- Web UI has no authentication — run behind a reverse proxy or Tailscale for remote access

---

*Maintained by [AgentForms](https://agentforms.io). Last updated: June 2026.*
