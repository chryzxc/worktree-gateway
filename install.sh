#!/bin/sh
# Install wtg (Worktree Gateway).
#
#   gh api repos/chryzxc/worktree-gateway/contents/install.sh -H 'Accept: application/vnd.github.raw' | sh
#   curl -fsSL https://raw.githubusercontent.com/chryzxc/worktree-gateway/main/install.sh | sh   # once public
#
# Environment:
#   WTG_VERSION       release tag to install (default: latest)
#   WTG_INSTALL_DIR   target directory (default: ~/.local/bin)
#   GITHUB_TOKEN      used when gh is not available (needed while the repo is private)
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

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
pattern="wtg_*_${os}_${arch}.tar.gz"

download() {
  if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    tag=""
    [ "$VERSION" = latest ] || tag="$VERSION"
    # shellcheck disable=SC2086
    gh release download $tag --repo "$REPO" --pattern "$pattern" --pattern checksums.txt --dir "$tmp" && return 0
    return 1
  fi
  command -v curl >/dev/null 2>&1 || return 1
  auth=""
  [ -n "${GITHUB_TOKEN:-}" ] && auth="Authorization: Bearer $GITHUB_TOKEN"
  if [ "$VERSION" = latest ]; then api="https://api.github.com/repos/$REPO/releases/latest"
  else api="https://api.github.com/repos/$REPO/releases/tags/$VERSION"; fi
  json=$(curl -fsSL ${auth:+-H "$auth"} "$api") || return 1
  for want in "_${os}_${arch}.tar.gz" "checksums.txt"; do
    # In each asset object the API "url" field precedes "name".
    line=$(printf '%s' "$json" | tr ',{}' '\n\n\n' | awk -v want="$want" '
      /"url": *"/ { u = $0; sub(/.*"url": *"/, "", u); sub(/".*/, "", u) }
      /"name": *"/ { n = $0; sub(/.*"name": *"/, "", n); sub(/".*/, "", n)
                     if (substr(n, length(n) - length(want) + 1) == want) { print u " " n; exit } }')
    url=${line%% *}
    name=${line#* }
    [ -n "$url" ] && [ -n "$name" ] || return 1
    curl -fsSL ${auth:+-H "$auth"} -H 'Accept: application/octet-stream' -o "$tmp/$name" "$url" || return 1
  done
}

say "Installing wtg ($VERSION, $os/$arch) into $DIR"
if download; then
  archive=$(ls "$tmp"/wtg_*.tar.gz)
  if command -v shasum >/dev/null 2>&1; then sum="shasum -a 256"; else sum="sha256sum"; fi
  (cd "$tmp" && grep "$(basename "$archive")" checksums.txt | $sum -c - >/dev/null) || die "checksum mismatch"
  tar -xzf "$archive" -C "$tmp"
  mkdir -p "$DIR"
  install -m 0755 "$tmp"/wtg_*/wtg "$DIR/wtg"
elif command -v go >/dev/null 2>&1; then
  say "No release download available (private repo without gh/GITHUB_TOKEN?); building from source with go"
  ref="$VERSION"; [ "$ref" = latest ] && ref=latest
  GOPRIVATE="github.com/$REPO" GOBIN="$DIR" go install "github.com/$REPO/cmd/wtg@$ref"
else
  die "could not download a release. Install and log in to the GitHub CLI (gh auth login), set GITHUB_TOKEN, or install Go."
fi

say "Installed $("$DIR/wtg" version)"
case ":$PATH:" in
  *":$DIR:"*) ;;
  *) printf '\nAdd %s to your PATH:\n  echo '\''export PATH="%s:$PATH"'\'' >> ~/.%src\n' "$DIR" "$DIR" "$(basename "${SHELL:-sh}")";;
esac
printf '\nGet started:\n  cd your-repo && wtg init && wtg run && wtg open\n'
