#!/bin/sh
# Install wtg (Worktree Gateway).
#
#   curl -fsSL https://raw.githubusercontent.com/chryzxc/worktree-gateway/main/install.sh | sh
#
# Environment:
#   WTG_VERSION       release tag to install, e.g. v0.1.0 (default: latest)
#   WTG_INSTALL_DIR   target directory (default: ~/.local/bin)
set -eu

REPO="chryzxc/worktree-gateway"
VERSION="${WTG_VERSION:-latest}"
DIR="${WTG_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '\033[1m%s\033[0m\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in darwin|linux) ;; *) die "unsupported OS: $os (build from source: go install github.com/$REPO/cmd/wtg@latest)";; esac
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; *) die "unsupported architecture: $arch";; esac

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL -o "$2" "$1"; }
  resolve() { curl -fsSLI -o /dev/null -w '%{url_effective}' "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
  resolve() { wget -S --spider "$1" 2>&1 | sed -n 's/^ *Location: *//p' | tail -1 | tr -d '\r'; }
else
  die "curl or wget is required"
fi

if [ "$VERSION" = latest ]; then
  # /releases/latest redirects to /releases/tag/<tag>.
  VERSION=$(resolve "https://github.com/$REPO/releases/latest")
  VERSION=${VERSION##*/}
  case "$VERSION" in v*) ;; *) die "could not determine the latest release";; esac
fi

name="wtg_${VERSION}_${os}_${arch}"
base="https://github.com/$REPO/releases/download/$VERSION"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "Installing wtg $VERSION ($os/$arch) into $DIR"
fetch "$base/$name.tar.gz" "$tmp/$name.tar.gz" || die "download failed: $base/$name.tar.gz"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: $base/checksums.txt"

if command -v shasum >/dev/null 2>&1; then sum="shasum -a 256"; else sum="sha256sum"; fi
(cd "$tmp" && grep " $name.tar.gz\$" checksums.txt | $sum -c - >/dev/null) || die "checksum mismatch for $name.tar.gz"

tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
mkdir -p "$DIR"
install -m 0755 "$tmp/$name/wtg" "$DIR/wtg"

say "Installed $("$DIR/wtg" version)"
if "$DIR/wtg" daemon status >/dev/null 2>&1; then
  printf '\nA wtg daemon is already running; load the new version with:\n  wtg daemon restart\n'
fi
case ":$PATH:" in
  *":$DIR:"*) ;;
  *) printf '\nAdd %s to your PATH:\n  echo '\''export PATH="%s:$PATH"'\'' >> ~/.%src\n' "$DIR" "$DIR" "$(basename "${SHELL:-sh}")";;
esac
printf '\nGet started:\n  cd your-repo && wtg init && wtg run && wtg open\n'
