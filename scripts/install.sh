#!/usr/bin/env bash
set -euo pipefail

# Sonar installer
# Usage: curl -sfL https://raw.githubusercontent.com/raskrebs/sonar/main/scripts/install.sh | bash

INSTALL_DIR="${SONAR_INSTALL_DIR:-$HOME/.local/bin}"
REPO="raskrebs/sonar"

# Colors (respect NO_COLOR)
if [ -z "${NO_COLOR:-}" ] && [ -t 1 ]; then
    BOLD='\033[1m'
    CYAN='\033[36m'
    GREEN='\033[32m'
    RED='\033[31m'
    DIM='\033[2m'
    RESET='\033[0m'
else
    BOLD='' CYAN='' GREEN='' RED='' DIM='' RESET=''
fi

info() { printf "${BOLD}${CYAN}sonar${RESET} %s\n" "$1"; }
success() { printf "${GREEN}✓${RESET} %s\n" "$1"; }
error() { printf "${RED}✗${RESET} %s\n" "$1" >&2; exit 1; }
dim() { printf "${DIM}%s${RESET}\n" "$1"; }

# Detect platform
detect_platform() {
    local os arch

    case "$(uname -s)" in
        Darwin) os="darwin" ;;
        Linux)  os="linux" ;;
        *)      error "Unsupported OS: $(uname -s)" ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64)  arch="amd64" ;;
        arm64|aarch64) arch="arm64" ;;
        *)             error "Unsupported architecture: $(uname -m)" ;;
    esac

    echo "${os}_${arch}"
}

PLATFORM="$(detect_platform)"
info "Detected platform: ${PLATFORM}"

# Find latest release
info "Fetching latest release..."
LATEST_URL="https://api.github.com/repos/${REPO}/releases/latest"

if command -v curl &>/dev/null; then
    RELEASE_JSON="$(curl -sfL "$LATEST_URL")" || error "Failed to fetch latest release. Check https://github.com/${REPO}/releases"
elif command -v wget &>/dev/null; then
    RELEASE_JSON="$(wget -qO- "$LATEST_URL")" || error "Failed to fetch latest release. Check https://github.com/${REPO}/releases"
else
    error "curl or wget is required"
fi

# Parse download URL for this platform
DOWNLOAD_URL="$(echo "$RELEASE_JSON" | grep -o "\"browser_download_url\": *\"[^\"]*${PLATFORM}[^\"]*\"" | head -1 | cut -d'"' -f4)"

if [ -z "$DOWNLOAD_URL" ]; then
    error "No binary found for ${PLATFORM}. Check https://github.com/${REPO}/releases"
fi

TAG="$(echo "$RELEASE_JSON" | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4)"
info "Downloading sonar ${TAG} for ${PLATFORM}..."

# Download and extract
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

if command -v curl &>/dev/null; then
    curl -sfL "$DOWNLOAD_URL" -o "$TMP_DIR/sonar.tar.gz"
else
    wget -qO "$TMP_DIR/sonar.tar.gz" "$DOWNLOAD_URL"
fi

tar xzf "$TMP_DIR/sonar.tar.gz" -C "$TMP_DIR"

# Install
mkdir -p "$INSTALL_DIR"

# Find the binary (might be at root or in a subdirectory)
BINARY="$(find "$TMP_DIR" -name sonar -type f | head -1)"
if [ -z "$BINARY" ]; then
    error "sonar binary not found in release archive"
fi

cp "$BINARY" "$INSTALL_DIR/sonar"
chmod +x "$INSTALL_DIR/sonar"
success "Installed sonar ${TAG} to $INSTALL_DIR/sonar"

# Install sonar-tray if present (macOS only)
TRAY_BINARY="$(find "$TMP_DIR" -name sonar-tray -type f | head -1)"
if [ -n "$TRAY_BINARY" ]; then
    cp "$TRAY_BINARY" "$INSTALL_DIR/sonar-tray"
    chmod +x "$INSTALL_DIR/sonar-tray"
    success "Installed sonar-tray to $INSTALL_DIR/sonar-tray"
fi

# Add to PATH if not already there
add_to_path() {
    local shell_config="$1"
    local label="$2"

    if [ ! -f "$shell_config" ]; then
        return 1
    fi

    if grep -q "$INSTALL_DIR" "$shell_config" 2>/dev/null; then
        dim "PATH already configured in $label"
        return 0
    fi

    printf '\n# sonar\nexport PATH="%s:$PATH"\n' "$INSTALL_DIR" >> "$shell_config"
    success "Added sonar to PATH in $label"
    return 0
}

if echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
    dim "sonar is already in PATH"
else
    modified=false
    current_shell="$(basename "${SHELL:-bash}")"

    case "$current_shell" in
        zsh)
            add_to_path "$HOME/.zshrc" "~/.zshrc" && modified=true
            ;;
        bash)
            if [ -f "$HOME/.bashrc" ]; then
                add_to_path "$HOME/.bashrc" "~/.bashrc" && modified=true
            elif [ -f "$HOME/.bash_profile" ]; then
                add_to_path "$HOME/.bash_profile" "~/.bash_profile" && modified=true
            fi
            ;;
    esac

    if [ "$modified" = true ]; then
        echo ""
        info "Restart your terminal or run:"
        dim "  source ~/.${current_shell}rc"
    fi
fi

echo ""
success "Done! Run 'sonar' to get started."
