# Contributing to Agent Relay

Thank you for considering contributing. We welcome bug reports, feature requests, documentation improvements, and code contributions.

## Before You Start

- Check [existing issues](https://github.com/agentforms/agent-relay/issues) and [open PRs](https://github.com/agentforms/agent-relay/pulls) to avoid duplicates.
- For major changes, open an issue first to discuss the approach.

## Reporting Bugs

Use the [Issues](https://github.com/agentforms/agent-relay/issues) template. Include:

1. **What happened** — exact error message or unexpected behavior
2. **What you expected** — the correct behavior
3. **Steps to reproduce** — minimal command that triggers the issue
4. **Environment** — OS, architecture, relay version (`relay version`)
5. **Config** — redacted `config.json` (remove password) if relevant

## Submitting Changes

### Development workflow

```bash
# Fork and clone
git clone https://github.com/YOUR_USERNAME/agent-relay.git
cd agent-relay

# Build
make build

# Run locally
./relay version
./relay setup
./relay send -t test@example.com -b "test"

# Test changes
make test

# Commit with conventional commit messages
git commit -m "feat: add direct TLS support for port 465"
```

### Commit message format

We follow [Conventional Commits](https://www.conventionalcommits.org/):

```
type: description

type: feat | fix | docs | chore | refactor | test
```

Examples:
- `feat: add BCC recipient mode`
- `fix: handle empty subject in sent log`
- `docs: clarify STARTTLS vs direct TLS`

### Code standards

- **Language:** Go 1.24+
- **Formatting:** `gofmt` — run before committing
- **Single file:** The entire relay is `main.go`. Keep it under 1000 lines. For larger features, extract to packages.
- **Error handling:** Use `fmt.Errorf("context: %w", err)` for wrapping
- **No external deps:** The relay uses only Go stdlib. If you need a library, explain why in the PR.

### Pull request checklist

- [ ] Code formatted with `gofmt`
- [ ] Changes tested on at least one architecture
- [ ] README updated if CLI flags or config changed
- [ ] CHANGELOG.md updated with the change
- [ ] No secrets or credentials in the diff

## Documentation

Docs live alongside code. If a feature isn't documented, it doesn't exist. Update README.md for:

- New commands or flags
- Config changes
- Architecture changes
- Troubleshooting entries for known issues

## Community

- Report security vulnerabilities via [SECURITY.md](SECURITY.md)
- Ask questions via [Discussions](https://github.com/agentforms/agent-relay/discussions)

## Code of Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Be respectful, constructive, and assume good faith.

---

*This project is maintained by [AgentForms](https://agentforms.io). We aim to respond to issues within 48 hours and review PRs within one week.*
