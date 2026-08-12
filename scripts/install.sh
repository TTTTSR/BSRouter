#!/bin/sh
#
# BSRouter bsr installer — Linux / macOS.
#
# Usage:
#   ./install.sh [--version <ver>] [--base-url <url>] [--prefix <dir>]
#                [--local <build-dir>] [--no-path]
#
#   --version <ver>   release version to download (default: latest)
#   --base-url <url>  download base URL (default: https://github.com/TTTTSR/BSRouter/releases/download)
#   --prefix <dir>    install under <dir>/bin (default: ~/.local)
#   --local <dir>     install from a local build directory instead of downloading
#   --no-path         do not modify the shell PATH
#
# The downloaded asset is <base-url>/<version>/bsr-<os>-<arch>.tar.gz and must
# contain the `gateway` binary and the `bsr` launcher script. With --local,
# <dir> must hold the built `gateway` (or `gateway.exe`) and a scripts/bsr file.
#
# Released under the MIT License.
set -u

VERSION="latest"
BASE_URL="https://github.com/TTTTSR/BSRouter/releases/download"
PREFIX="${HOME}/.local"
LOCAL=""
NO_PATH=0

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version) VERSION=${2:?}; shift 2 ;;
        --base-url) BASE_URL=${2:?}; shift 2 ;;
        --prefix) PREFIX=${2:?}; shift 2 ;;
        --local) LOCAL=${2:?}; shift 2 ;;
        --no-path) NO_PATH=1; shift ;;
        -h|--help)
            sed -n '2,12p' "$0"
            exit 0
            ;;
        *)
            echo "install.sh: unknown option: $1" >&2
            echo "Try 'install.sh --help'." >&2
            exit 2
            ;;
    esac
done

BIN_DIR="$PREFIX/bin"

echo "==> BSRouter bsr installer"

# Windows under Git Bash/MSYS/Cygwin? Direct to install.ps1.
os=$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')
case "$os" in
    mingw*|msys*|cygwin*)
        echo "Detected Windows (Git Bash/MSYS/Cygwin). On Windows use:" >&2
        echo "    powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1" >&2
        exit 1
        ;;
    darwin*) os="darwin" ;;
    linux*)  os="linux" ;;
    *)
        echo "install.sh: unsupported OS: $os" >&2
        exit 1
        ;;
esac

arch=$(uname -m 2>/dev/null)
case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *)
        echo "install.sh: unsupported architecture: $arch" >&2
        exit 1
        ;;
esac

# resolve_latest 把 VERSION=latest 解析为最新 release 的真实标签名
# (GitHub API releases/latest)。资产按版本命名(bsr-v0.2.0-linux-amd64.tar.gz),
# 不能直接按 "latest" 字面文件名下载。
resolve_latest() {
    owner=$(printf '%s' "$BASE_URL" | awk -F/ '{print $(NF-3)}')
    repo=$(printf '%s' "$BASE_URL" | awk -F/ '{print $(NF-2)}')
    api="https://api.github.com/repos/$owner/$repo/releases/latest"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL -H "User-Agent: bsr-installer" "$api" 2>/dev/null \
            | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1
    elif command -v wget >/dev/null 2>&1; then
        wget -qO- "$api" 2>/dev/null \
            | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1
    fi
}

mkdir -p "$BIN_DIR" || { echo "install.sh: cannot create $BIN_DIR" >&2; exit 1; }

if [ -n "$LOCAL" ]; then
    echo "==> Installing from local build: $LOCAL"
    gateway_src="$LOCAL/gateway"
    [ -x "$gateway_src" ] || gateway_src="$LOCAL/gateway.exe"
    bsr_src="$LOCAL/scripts/bsr"
    [ -f "$bsr_src" ] || bsr_src="$LOCAL/bsr"
    if [ ! -f "$gateway_src" ]; then
        echo "install.sh: no gateway binary ('gateway'/'gateway.exe') in $LOCAL" >&2
        exit 1
    fi
    if [ ! -f "$bsr_src" ]; then
        echo "install.sh: no 'bsr' launcher in $LOCAL/scripts" >&2
        exit 1
    fi
    cp "$gateway_src" "$BIN_DIR/gateway"
    cp "$bsr_src" "$BIN_DIR/bsr"
    chmod +x "$BIN_DIR/gateway" "$BIN_DIR/bsr"
else
    if [ "$VERSION" = "latest" ]; then
        VERSION=$(resolve_latest)
        if [ -z "$VERSION" ]; then
            echo "install.sh: cannot resolve latest release from $BASE_URL" >&2
            exit 1
        fi
        echo "==> Resolved latest release: $VERSION"
    fi
    asset="bsr-$VERSION-$os-$arch.tar.gz"
    url="$BASE_URL/$VERSION/$asset"
    echo "==> Downloading $asset"
    echo "    $url"
    tmp=$(mktemp -d) || exit 1
    trap 'rm -rf "$tmp"' EXIT
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$tmp/$asset" || { echo "install.sh: download failed: $url" >&2; exit 1; }
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$url" -O "$tmp/$asset" || { echo "install.sh: download failed: $url" >&2; exit 1; }
    else
        echo "install.sh: need curl or wget to download" >&2
        exit 1
    fi
    tar -xzf "$tmp/$asset" -C "$tmp" || { echo "install.sh: cannot extract $asset" >&2; exit 1; }
    cp "$tmp/gateway" "$BIN_DIR/gateway"
    cp "$tmp/bsr" "$BIN_DIR/bsr"
    chmod +x "$BIN_DIR/gateway" "$BIN_DIR/bsr"
fi

echo "==> Installed:"
echo "    gateway : $BIN_DIR/gateway"
echo "    bsr     : $BIN_DIR/bsr"

if [ "$NO_PATH" -eq 0 ]; then
    case ":$PATH:" in
        *":$BIN_DIR:"*) : ;;   # already on PATH
        *)
            rc=""
            if [ -n "${SHELL:-}" ]; then
                case "$SHELL" in
                    *bash) rc="$HOME/.bashrc" ;;
                    *zsh)  rc="$HOME/.zshrc" ;;
                esac
            fi
            [ -z "$rc" ] && rc="$HOME/.profile"
            line="export PATH=\"$BIN_DIR:\$PATH\""
            if [ -f "$rc" ] && grep -qF "$BIN_DIR" "$rc"; then
                : # already present
            else
                echo "$line" >> "$rc"
            fi
            echo "==> Added $BIN_DIR to PATH in $rc"
            echo "    Run 'source $rc' or open a new terminal."
            ;;
    esac
else
    echo "==> PATH modification skipped (--no-path). Add $BIN_DIR to PATH manually."
fi

echo "==> Done. Use: bsr start"
