#!/bin/sh
# squish installer: grabs the right binary for this machine from the
# latest release and puts it on your PATH.
set -e
case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "squish: no release for $(uname -s) yet" >&2; exit 1 ;;
esac
m=$(uname -m)
case "$m" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "squish: no release for $m yet" >&2; exit 1 ;;
esac
url="https://github.com/aloysiusvonfck/squish/releases/latest/download/squish-$os-$arch"
dest="$HOME/.local/bin"
mkdir -p "$dest"
echo "downloading squish for $os/$arch"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$url" -o "$dest/squish"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$dest/squish" "$url"
else
  echo "need curl or wget" >&2; exit 1
fi
chmod +x "$dest/squish"
case ":$PATH:" in
  *":$dest:"*) ;;
  *) echo "installed: $dest/squish"
     echo "note: add $dest to PATH:  export PATH=\"$dest:\$PATH\"" ;;
esac
"$dest/squish" help 2>/dev/null | head -1 || true
echo "done. next: squish reclaim"
