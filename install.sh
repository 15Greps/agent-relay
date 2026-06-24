#!/usr/bin/env bash
# agent-relay installer for Linux/macOS
# Usage:
#   curl -fsSL https://relay.agentforms.io/install.sh | bash
#   sudo: curl -fsSL https://relay.agentforms.io/install.sh | sudo bash

set -euo pipefail

VERSION="${RELAY_VERSION:-0.2.6}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
REPO_URL="https://github.com/15Greps/agent-relay/releases/download/v${VERSION}"

detect_platform() {
    local os arch
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    arch="$(uname -m)"

    case "$arch" in
        x86_64|amd64) arch="amd64" ;;
        arm64|aarch64) arch="arm64" ;;
        armv7l) arch="armv7" ;;
        *) echo "Error: unsupported architecture $arch" >&2; exit 1 ;;
    esac

    case "$os" in
        linux) PLATFORM="linux-${arch}" ;;
        darwin) PLATFORM="darwin-${arch}" ;;
        *) echo "Error: unsupported OS $os" >&2; exit 1 ;;
    esac
}

main() {
    detect_platform
    local BINARY="relay-${PLATFORM}"
    local URL="${REPO_URL}/${BINARY}"

    echo "agent-relay v${VERSION} installer"
    echo "Platform: ${PLATFORM}"
    echo "Install dir: ${INSTALL_DIR}"
    echo ""

    # Create install directory
    mkdir -p "${INSTALL_DIR}"

    # Download
    echo "Downloading ${BINARY}..."
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL -o "${INSTALL_DIR}/relay" "${URL}"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "${INSTALL_DIR}/relay" "${URL}"
    else
        echo "Error: curl or wget required" >&2
        exit 1
    fi

    # Set permissions
    chmod +x "${INSTALL_DIR}/relay"

    # Verify
    if "${INSTALL_DIR}/relay" version >/dev/null 2>&1; then
        echo ""
        echo "✓ Installed relay to ${INSTALL_DIR}/relay"
        echo ""
        if ! echo "$PATH" | grep -q "${INSTALL_DIR}"; then
            echo "Add ${INSTALL_DIR} to your PATH:"
            echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
            echo "  (add to ~/.bashrc or ~/.zshrc)"
        fi
    else
        echo "Error: downloaded binary failed verification" >&2
        rm -f "${INSTALL_DIR}/relay"
        exit 1
    fi
}

main "$@"
