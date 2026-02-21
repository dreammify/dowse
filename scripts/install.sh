#!/usr/bin/env bash
#
# Dowse installation script
# Usage: curl -fsSL https://raw.githubusercontent.com/dreammify/dowse/main/scripts/install.sh | bash
#

set -e

REPO="dreammify/dowse"
BINARY="dowse"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() {
    echo -e "${BLUE}==>${NC} $1"
}

log_success() {
    echo -e "${GREEN}==>${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}==>${NC} $1"
}

log_error() {
    echo -e "${RED}Error:${NC} $1" >&2
}

detect_platform() {
    local os arch

    case "$(uname -s)" in
        Darwin)  os="darwin" ;;
        Linux)   os="linux" ;;
        MINGW*|MSYS*|CYGWIN*)
            os="windows" ;;
        *)
            log_error "Unsupported operating system: $(uname -s)"
            exit 1
            ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64)    arch="amd64" ;;
        aarch64|arm64)   arch="arm64" ;;
        *)
            log_error "Unsupported architecture: $(uname -m)"
            exit 1
            ;;
    esac

    echo "${os} ${arch}"
}

# Re-sign binary on macOS to avoid Gatekeeper delays
resign_for_macos() {
    local binary_path=$1

    if [[ "$(uname -s)" != "Darwin" ]]; then
        return 0
    fi

    if ! command -v codesign &> /dev/null; then
        return 0
    fi

    log_info "Re-signing binary for macOS..."
    codesign --remove-signature "$binary_path" 2>/dev/null || true
    if codesign --force --sign - "$binary_path"; then
        log_success "Binary re-signed successfully"
    else
        log_warning "Re-signing failed (non-fatal)"
    fi
}

http_get() {
    local url=$1
    local output=$2

    if command -v curl &> /dev/null; then
        if [ -n "$output" ]; then
            curl -fsSL -o "$output" "$url"
        else
            curl -fsSL "$url"
        fi
    elif command -v wget &> /dev/null; then
        if [ -n "$output" ]; then
            wget -qO "$output" "$url"
        else
            wget -qO- "$url"
        fi
    else
        log_error "Neither curl nor wget found. Please install one of them."
        exit 1
    fi
}

install_from_release() {
    local os=$1
    local arch=$2

    log_info "Fetching latest release..."
    local release_json
    release_json=$(http_get "https://api.github.com/repos/${REPO}/releases/latest" "")

    local version
    version=$(echo "$release_json" | grep '"tag_name"' | sed -E 's/.*"tag_name": "([^"]+)".*/\1/')

    if [ -z "$version" ]; then
        log_error "Failed to determine latest version"
        return 1
    fi

    log_info "Latest version: $version"

    # Build asset name matching release.yml naming convention
    local asset_name="${BINARY}-${os}-${arch}"
    if [ "$os" = "windows" ]; then
        asset_name="${asset_name}.exe"
    fi

    # Verify the asset exists in the release
    if ! echo "$release_json" | grep -Fq "\"name\": \"${asset_name}\""; then
        log_warning "No prebuilt binary for ${os}/${arch}"
        return 1
    fi

    local download_url="https://github.com/${REPO}/releases/download/${version}/${asset_name}"

    local tmp_dir
    tmp_dir=$(mktemp -d)
    local tmp_binary="${tmp_dir}/${BINARY}"

    log_info "Downloading ${asset_name}..."
    if ! http_get "$download_url" "$tmp_binary"; then
        log_error "Download failed"
        rm -rf "$tmp_dir"
        return 1
    fi

    chmod +x "$tmp_binary"

    # Determine install location
    local install_dir
    if [[ -d /usr/local/bin && -w /usr/local/bin ]]; then
        install_dir="/usr/local/bin"
    else
        install_dir="$HOME/.local/bin"
        mkdir -p "$install_dir"
    fi

    log_info "Installing to ${install_dir}..."
    if [[ -w "$install_dir" ]]; then
        mv "$tmp_binary" "$install_dir/${BINARY}"
    else
        sudo mv "$tmp_binary" "$install_dir/${BINARY}"
    fi

    resign_for_macos "$install_dir/${BINARY}"

    rm -rf "$tmp_dir"

    log_success "${BINARY} ${version} installed to ${install_dir}/${BINARY}"

    if [[ ":$PATH:" != *":$install_dir:"* ]]; then
        log_warning "$install_dir is not in your PATH"
        echo ""
        echo "Add this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
        echo "  export PATH=\"\$PATH:$install_dir\""
        echo ""
    fi

    return 0
}

install_with_go() {
    if ! command -v go &> /dev/null; then
        return 1
    fi

    log_info "Falling back to 'go install'..."

    if ! go install "github.com/${REPO}/cmd/${BINARY}@latest"; then
        log_error "'go install' failed"
        return 1
    fi

    local gobin
    gobin=$(go env GOBIN 2>/dev/null || true)
    if [ -z "$gobin" ]; then
        gobin="$(go env GOPATH)/bin"
    fi

    resign_for_macos "$gobin/${BINARY}"

    log_success "${BINARY} installed via go install"

    if [[ ":$PATH:" != *":$gobin:"* ]]; then
        log_warning "$gobin is not in your PATH"
        echo ""
        echo "Add this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
        echo "  export PATH=\"\$PATH:$gobin\""
        echo ""
    fi

    return 0
}

verify_installation() {
    if command -v "$BINARY" &> /dev/null; then
        log_success "${BINARY} is installed and ready!"
        echo ""
        "$BINARY" --version 2>/dev/null || echo "${BINARY} (unknown version)"
        echo ""
        echo "Get started:"
        echo "  ${BINARY} start"
        echo "  ${BINARY} diagnostics <file>"
        echo ""
        return 0
    else
        log_error "${BINARY} was installed but is not in your PATH"
        return 1
    fi
}

main() {
    echo ""
    echo "Dowse Installer"
    echo "LSP bridge for AI agents"
    echo ""

    log_info "Detecting platform..."
    local platform
    platform=$(detect_platform)
    read -r os arch <<< "$platform"
    log_info "Platform: ${os}/${arch}"

    # Try prebuilt binary first
    if install_from_release "$os" "$arch"; then
        verify_installation
        exit 0
    fi

    # Fall back to go install
    log_warning "Prebuilt binary not available, trying go install..."
    if install_with_go; then
        verify_installation
        exit 0
    fi

    # All methods failed
    log_error "Installation failed"
    echo ""
    echo "To install manually:"
    echo "  1. Download a binary from https://github.com/${REPO}/releases/latest"
    echo "  2. Make it executable: chmod +x ${BINARY}-*"
    echo "  3. Move it to your PATH: mv ${BINARY}-* /usr/local/bin/${BINARY}"
    echo ""
    echo "Or install with Go:"
    echo "  go install github.com/${REPO}/cmd/${BINARY}@latest"
    echo ""
    exit 1
}

main "$@"
