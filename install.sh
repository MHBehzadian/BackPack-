#!/usr/bin/env bash
# BackPack+ installer — run on the KHAREJ server as root.
#   bash <(curl -fsSL https://raw.githubusercontent.com/MHBehzadian/BackPack-/main/install.sh)
set -euo pipefail

REPO="MHBehzadian/BackPack-"
BIN=/usr/local/bin/backpack-plus
CONF_DIR=/etc/backpack-plus
UNIT=/etc/systemd/system/backpack-plus.service

[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }

case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)"; exit 1 ;;
esac

TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
URL="https://github.com/$REPO/releases/latest/download"
echo "downloading backpack-plus-linux-$ARCH ..."
if curl -fsSL -o "$TMP/backpack-plus-linux-$ARCH" "$URL/backpack-plus-linux-$ARCH" &&
   curl -fsSL -o "$TMP/SHA256SUMS" "$URL/SHA256SUMS"; then
  (cd "$TMP" && grep " backpack-plus-linux-$ARCH\$" SHA256SUMS | sha256sum -c -)
  install -m 0755 "$TMP/backpack-plus-linux-$ARCH" "$BIN"
elif command -v go >/dev/null 2>&1; then
  echo "no release found; building from source with $(go version)"
  git clone --depth 1 "https://github.com/$REPO" "$TMP/src"
  (cd "$TMP/src" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$BIN" ./cmd/backpack-plus)
else
  echo "no release found and Go is not installed; install Go 1.22+ or publish a release first"; exit 1
fi

mkdir -p "$CONF_DIR"
if [ ! -f "$CONF_DIR/config.json" ]; then
  curl -fsSL -o "$CONF_DIR/config.json" "https://raw.githubusercontent.com/$REPO/main/config.example.json"
  chmod 600 "$CONF_DIR/config.json"
  NEW_CONF=1
fi

curl -fsSL -o "$UNIT" "https://raw.githubusercontent.com/$REPO/main/deploy/backpack-plus.service"
systemctl daemon-reload

echo
echo "installed: $($BIN version)"
if [ "${NEW_CONF:-0}" = 1 ]; then
  echo "1) edit $CONF_DIR/config.json (servers, arvan api key / domain / record, telegram)"
else
  echo "1) existing $CONF_DIR/config.json kept"
fi
echo "2) add the probe port to BOTH tunnels on the Iran servers (see README)"
echo "3) backpack-plus check        # tests arvan, both tunnels and the bot"
echo "4) systemctl enable --now backpack-plus && journalctl -u backpack-plus -f"
