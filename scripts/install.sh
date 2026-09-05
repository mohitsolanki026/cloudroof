#!/usr/bin/env sh
# Bosun installer.  curl -sSL https://get.bosun.sh | sh
#
# Downloads the latest release binary for this OS/arch and installs it to
# /usr/local/bin (or ~/.local/bin without root). Override:
#   BOSUN_REPO   owner/repo to fetch from        (default bosun-sh/bosun)
#   BOSUN_VERSION  tag to install                (default: latest)
#   BOSUN_BIN_DIR  install directory
set -eu

REPO="${BOSUN_REPO:-bosun-sh/bosun}"
VERSION="${BOSUN_VERSION:-latest}"

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
  [ -n "$VERSION" ] || die "could not resolve the latest release from github.com/$REPO — set BOSUN_VERSION, or build from source"
fi
ver="${VERSION#v}"

tarball="bosun_${ver}_${os}_${arch}.tar.gz"
url="https://github.com/$REPO/releases/download/$VERSION/$tarball"

# Pick an install dir we can actually write to.
if [ -n "${BOSUN_BIN_DIR:-}" ]; then bindir="$BOSUN_BIN_DIR"
elif [ -w /usr/local/bin ] 2>/dev/null; then bindir=/usr/local/bin
elif [ "$(id -u)" = 0 ]; then bindir=/usr/local/bin
else bindir="$HOME/.local/bin"; fi
mkdir -p "$bindir"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
say "Downloading $tarball …"
curl -fSL "$url" -o "$tmp/$tarball" || die "download failed: $url"
tar -C "$tmp" -xzf "$tmp/$tarball" || die "extract failed"
install -m 0755 "$tmp/bosun" "$bindir/bosun" 2>/dev/null || { mv "$tmp/bosun" "$bindir/bosun"; chmod 0755 "$bindir/bosun"; }

say ""
say "Installed bosun $VERSION to $bindir/bosun"
case ":$PATH:" in
  *":$bindir:"*) ;;
  *) say "Note: $bindir is not on your PATH. Add it, or run $bindir/bosun directly." ;;
esac
say "Start it with:  bosun            # then open http://localhost:7070"
