#!/usr/bin/env bash
# BackPack+ installer — run on the KHAREJ server as root:
#   bash <(curl -fsSL https://raw.githubusercontent.com/MHBehzadian/BackPack-/main/install.sh)
#
# Installs the binary and the systemd unit, then (on a terminal) runs the setup
# wizard and the self-check, and asks before starting the service. Re-running it
# upgrades the binary and keeps the existing config.
set -euo pipefail

REPO="MHBehzadian/BackPack-"
REF=${BPP_REF:-main}
RAW="https://raw.githubusercontent.com/$REPO/$REF"
BIN=/usr/local/bin/backpack-plus
CONF_DIR=/etc/backpack-plus
CONF=$CONF_DIR/config.json
UNIT=/etc/systemd/system/backpack-plus.service

[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }
case "$(uname -m)" in
x86_64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) echo "unsupported architecture: $(uname -m)"; exit 1 ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
URL="https://github.com/$REPO/releases/latest/download"
echo "downloading backpack-plus-linux-$ARCH ..."
if curl -fsSL -o "$TMP/backpack-plus-linux-$ARCH" "$URL/backpack-plus-linux-$ARCH" &&
	curl -fsSL -o "$TMP/SHA256SUMS" "$URL/SHA256SUMS"; then
	(cd "$TMP" && grep " backpack-plus-linux-$ARCH\$" SHA256SUMS | sha256sum -c -)
	install -m 0755 "$TMP/backpack-plus-linux-$ARCH" "$BIN.new"
elif command -v go >/dev/null 2>&1; then
	echo "no release found; building from source ($REF) with $(go version)"
	git clone --depth 1 --branch "$REF" "https://github.com/$REPO" "$TMP/src"
	(cd "$TMP/src" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$BIN.new" ./cmd/backpack-plus)
else
	echo "no release found and Go is not installed; install Go 1.22+ or publish a release first"
	exit 1
fi
if [ -f "$CONF" ] && ! "$BIN.new" -config "$CONF" validate; then
	rm -f "$BIN.new"
	echo "the new version does not accept $CONF (above); nothing was changed"
	exit 1
fi
mv -f "$BIN.new" "$BIN"

mkdir -p "$CONF_DIR"
chmod 700 "$CONF_DIR"
curl -fsSL -o "$UNIT" "$RAW/deploy/backpack-plus.service"
curl -fsSL -o "$CONF_DIR/iran-setup.sh" "$RAW/iran-setup.sh" && chmod 755 "$CONF_DIR/iran-setup.sh" || true
systemctl daemon-reload
echo "installed: $($BIN version)"

if systemctl is-active --quiet backpack-plus; then
	# Upgrade: the config was validated with the new binary above.
	systemctl restart backpack-plus
	echo "backpack-plus restarted with the new version"
	exit 0
fi

if [ ! -t 0 ]; then
	echo "not on a terminal: run 'backpack-plus setup', then 'backpack-plus check' and 'systemctl enable --now backpack-plus'"
	exit 0
fi

cat <<EOF

Before this step, run iran-setup.sh on EVERY Iran server; it prints the probe
ports you are asked for below. A copy is at $CONF_DIR/iran-setup.sh:
    scp $CONF_DIR/iran-setup.sh root@IRAN_SERVER:
    ssh root@IRAN_SERVER bash iran-setup.sh

EOF
read -r -p "Run the setup wizard now? [Y/n]: " a
[[ ${a:-y} =~ ^[Yy] ]] || exit 0
$BIN -config "$CONF" setup

echo
echo "== self-check (starts a temporary echo server and probes every tunnel) =="
if $BIN -config "$CONF" check; then
	read -r -p "Start BackPack+ now and on every boot? [Y/n]: " a
	if [[ ${a:-y} =~ ^[Yy] ]]; then
		systemctl enable --now backpack-plus
		echo "started. Logs: journalctl -u backpack-plus -f"
	fi
else
	echo
	echo "the check found problems (above). Fix them, re-run 'backpack-plus check',"
	echo "then: systemctl enable --now backpack-plus"
fi
