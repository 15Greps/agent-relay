.PHONY: build build-all clean install test help

# Default binary name
BINARY=relay
VERSION=0.3.1
GO=go
# Relay token injected at build time for managed relay mode.
# Prefer RELAY_TOKEN_FILE (file path) for special chars, or RELAY_TOKEN env var.
RELAY_TOKEN_FILE ?=
ifdef RELAY_TOKEN_FILE
  RELAY_TOKEN := $(shell cat $(RELAY_TOKEN_FILE))
endif
RELAY_TOKEN ?= AGENTFORMS_RELAY_TOKEN
GOFLAGS=-ldflags="-s -w -X main.relayToken=$(RELAY_TOKEN)"

# Detect OS/ARCH for local build
GOOS=$(shell go env GOOS)
GOARCH=$(shell go env GOARCH)

help:
	@echo "agent-relay build targets"
	@echo ""
	@echo "  make build        - Build for current platform"
	@echo "  make build-all    - Build for all platforms (Linux, macOS, Windows)"
	@echo "  make release      - Build all + package into dist/ with checksums"
	@echo "  make install      - Install to ~/.local/bin"
	@echo "  make test         - Run tests"
	@echo "  make clean        - Remove build artifacts"
	@echo ""
	@echo "Variables:"
	@echo "  RELAY_TOKEN      Token for managed relay (default: placeholder)"
	@echo "  RELAY_TOKEN_FILE File containing token (safer for special chars)"

build:
	@echo "Building $(BINARY) for $(GOOS)/$(GOARCH)..."
	$(GO) build $(GOFLAGS) -o $(BINARY) .
	@echo "✓ Built $(BINARY)"

build-all:
	@echo "Building for all architectures..."
	@echo ""
	@echo "🐧 Linux..."
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o $(BINARY)-linux-amd64 .
	@echo "  ✓ linux-amd64"
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -o $(BINARY)-linux-arm64 .
	@echo "  ✓ linux-arm64"
	GOOS=linux GOARCH=arm GOARM=7 $(GO) build $(GOFLAGS) -o $(BINARY)-linux-armv7 .
	@echo "  ✓ linux-armv7"
	@echo ""
	@echo "🍎 macOS..."
	GOOS=darwin GOARCH=amd64 $(GO) build $(GOFLAGS) -o $(BINARY)-darwin-amd64 .
	@echo "  ✓ darwin-amd64"
	GOOS=darwin GOARCH=arm64 $(GO) build $(GOFLAGS) -o $(BINARY)-darwin-arm64 .
	@echo "  ✓ darwin-arm64"
	@echo ""
	@echo "🪟 Windows..."
	GOOS=windows GOARCH=amd64 $(GO) build $(GOFLAGS) -o $(BINARY)-windows-amd64.exe .
	@echo "  ✓ windows-amd64.exe"
	@echo ""
	@echo "Binary sizes:"
	@ls -lh $(BINARY)-* 2>/dev/null || true

install: build
	@mkdir -p $(HOME)/.local/bin
	@cp $(BINARY) $(HOME)/.local/bin/$(BINARY)
	@echo "✓ Installed $(BINARY) to $(HOME)/.local/bin/$(BINARY)"

release: clean
	@echo "Creating release package (v$(VERSION))..."
	@make build-all
	@mkdir -p dist
	@cp $(BINARY)-* dist/
	@cp README.md dist/
	@cp LICENSE dist/
	@cp install.sh dist/
	@cp install.ps1 dist/
	@cd dist && sha256sum * > SHA256SUMS
	@echo ""
	@echo "Release package in dist/:"
	@ls -lh dist/
	@echo ""
	@echo "Checksums:"
	@cat dist/SHA256SUMS

test:
	@echo "Running tests..."
	$(GO) vet ./...
	@echo "✓ Tests passed"

clean:
	rm -f $(BINARY) $(BINARY)-*
	rm -rf dist/
	@echo "✓ Cleaned build artifacts"
