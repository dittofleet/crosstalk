#!/bin/sh
set -eu

REPO="dittofleet/crosstalk"
DEST="${CROSSTALK_INSTALL_DIR:-$HOME/.local/bin}"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  darwin) ;;
  *) echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

ARCH=$(uname -m)
case "$ARCH" in
  arm64) ARCH=arm64 ;;
  x86_64) ARCH=x64 ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

ASSET="crosstalk-${OS}-${ARCH}"
URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"

mkdir -p "$DEST"
# Next to the binary, so the mv below is a rename: a running daemon keeps
# its old file and the next start gets the new one.
TMP=$(mktemp "$DEST/.crosstalk.XXXXXX")
trap 'rm -f "$TMP"' EXIT

echo "Downloading $URL..." >&2
curl -fsSL "$URL" -o "$TMP"
chmod +x "$TMP"
mv "$TMP" "$DEST/crosstalk"
echo "Installed crosstalk to $DEST/crosstalk" >&2

case ":$PATH:" in
  *":$DEST:"*) ;;
  *) echo "Note: $DEST is not in \$PATH. Add it to your shell profile to use crosstalk." >&2 ;;
esac

CONFIG_FILE="${XDG_CONFIG_HOME:-$HOME/.config}/crosstalk/config.json"

# A machine that has joined already only needs its daemon moved onto the
# new binary. start also sets the background service up where there is
# none yet, as on a machine that has only run the daemon by hand.
if [ -f "$CONFIG_FILE" ]; then
  echo "This machine has already joined a hub (left as it is)" >&2
  "$DEST/crosstalk" start
  exit 0
fi

# The hub comes from CROSSTALK_HUB (which, with CROSSTALK_KEY, makes a fresh
# remote machine a single curl-pipe), or from a prompt when a terminal is
# attached. Under `curl | sh` stdin is the script itself, so the prompt goes
# through /dev/tty. The subshell probe skips it when there is no terminal
# (CI, containers) rather than hanging or tripping set -e.
HUB="${CROSSTALK_HUB:-}"
if [ -z "$HUB" ] && ( : < /dev/tty ) 2>/dev/null; then
  printf "Hub URL (empty to join later): " > /dev/tty
  read -r HUB < /dev/tty || HUB=""
fi

if [ -z "$HUB" ]; then
  echo "Run 'crosstalk join <hub url>' to connect this machine to your hub." >&2
  exit 0
fi

# join asks for the key itself, without showing it, unless CROSSTALK_KEY
# is set.
"$DEST/crosstalk" join "$HUB"
