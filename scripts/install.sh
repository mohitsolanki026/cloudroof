#!/usr/bin/env sh
# CloudRoof installer.  curl -sSL https://get.cloudroof.sh | sh
#
# Downloads the latest release binary for this OS/arch and installs it to
# /usr/local/bin (or ~/.local/bin without root). Override:
#   CLOUDROOF_REPO   owner/repo to fetch from        (default cloudroof-sh/cloudroof)
#   CLOUDROOF_VERSION  tag to install                (default: latest)
#   CLOUDROOF_BIN_DIR  install directory
set -eu

REPO="${CLOUDROOF_REPO:-cloudroof-sh/cloudroof}"
VERSION="${CLOUDROOF_VERSION:-latest}"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture: $arch (build from source: https://github.com/$REPO)" ;;
esac
case "$os" in
  linux|darwin) ;;
  *) die "unsupported OS: $os (build from source)" ;;
esac

# Resolve the tag.
if [ "$VERSION" = latest ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep -m1 '"tag_name"' | cut -d'"' -f4) || true
  [ -n "$VERSION" ] || die "could not resolve the latest release from github.com/$REPO — set CLOUDROOF_VERSION, or build from source"
fi
ver="${VERSION#v}"

tarball="cloudroof_${ver}_${os}_${arch}.tar.gz"
url="https://github.com/$REPO/releases/download/$VERSION/$tarball"

# Pick an install dir we can actually write to.
if [ -n "${CLOUDROOF_BIN_DIR:-}" ]; then bindir="$CLOUDROOF_BIN_DIR"
elif [ -w /usr/local/bin ] 2>/dev/null; then bindir=/usr/local/bin
elif [ "$(id -u)" = 0 ]; then bindir=/usr/local/bin
else bindir="$HOME/.local/bin"; fi
mkdir -p "$bindir"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
say "Downloading $tarball …"
curl -fSL "$url" -o "$tmp/$tarball" || die "download failed: $url"
tar -C "$tmp" -xzf "$tmp/$tarball" || die "extract failed"
install -m 0755 "$tmp/cloudroof" "$bindir/cloudroof" 2>/dev/null || { mv "$tmp/cloudroof" "$bindir/cloudroof"; chmod 0755 "$bindir/cloudroof"; }

say ""
say "Installed cloudroof $VERSION to $bindir/cloudroof"
case ":$PATH:" in
  *":$bindir:"*) ;;
  *) say "Note: $bindir is not on your PATH. Add it, or run $bindir/cloudroof directly." ;;
esac
say "Start it with:  cloudroof            # then open http://localhost:7070"
