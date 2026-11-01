#!/usr/bin/env bash
# ZeniFlow agent installer (Linux, systemd). Run as root.
#
#   curl -fsSL https://raw.githubusercontent.com/ZeniFlow/zeniflow-agent/main/install.sh | sudo bash -s -- \
#     --server panel.example.com:8443 --token TOKEN --ca-sha256 HEX
#
# The command with your values is shown in the panel (Admin → Nodes → Add node).
# Options: --version vX.Y.Z (default: latest release), --binary PATH|URL (skip the download), --uninstall
set -euo pipefail

REPO="ZeniFlow/zeniflow-agent"
SERVER="" TOKEN="" CA="" VERSION="latest" BINARY="" UNINSTALL=0
while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="${2:-}"; shift 2 ;;
    --token) TOKEN="${2:-}"; shift 2 ;;
    --ca-sha256) CA="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --binary) BINARY="${2:-}"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ "$(id -u)" = 0 ] || { echo "run as root (sudo)" >&2; exit 1; }
[ "$(uname -s)" = Linux ] || { echo "only Linux is supported" >&2; exit 1; }
command -v systemctl >/dev/null || { echo "systemd is required" >&2; exit 1; }

BIN=/usr/local/bin/zeniflow-agent
UNIT=/etc/systemd/system/zeniflow-agent.service

if [ "$UNINSTALL" = 1 ]; then
  systemctl disable --now zeniflow-agent 2>/dev/null || true
  rm -f "$UNIT" "$BIN"
  systemctl daemon-reload
  echo "Agent removed. State directory /etc/zeniflow-agent was kept (delete it to enroll the node again)."
  exit 0
fi

[ -n "$SERVER" ] && [ -n "$TOKEN" ] && [ -n "$CA" ] || { echo "need --server, --token and --ca-sha256 (copy the command from the panel)" >&2; exit 2; }

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
if [ -n "$BINARY" ]; then
  case "$BINARY" in
    http://*) echo "refusing to download over plain http" >&2; exit 1 ;;
    https://*) curl -fsSL "$BINARY" -o "$TMP/agent" ;;
    *) cp "$BINARY" "$TMP/agent" ;;
  esac
else
  ASSET="zeniflow-agent-linux-$ARCH"
  if [ "$VERSION" = latest ]; then BASE="https://github.com/$REPO/releases/latest/download"; else BASE="https://github.com/$REPO/releases/download/$VERSION"; fi
  curl -fsSL "$BASE/$ASSET" -o "$TMP/agent"
  curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt"
  WANT="$(grep " $ASSET\$" "$TMP/checksums.txt" | cut -d' ' -f1)"
  GOT="$(sha256sum "$TMP/agent" | cut -d' ' -f1)"
  [ -n "$WANT" ] && [ "$WANT" = "$GOT" ] || { echo "checksum mismatch for $ASSET, aborting" >&2; exit 1; }
fi
install -m 755 "$TMP/agent" "$BIN.new" && mv "$BIN.new" "$BIN"

# The token is single-use: do not enroll again if this node is already registered.
if [ ! -d /etc/zeniflow-agent ] || [ -z "$(ls -A /etc/zeniflow-agent 2>/dev/null)" ]; then
  "$BIN" enroll --server "$SERVER" --token "$TOKEN" --ca-sha256 "$CA"
fi

cat > "$UNIT" <<'UNITEOF'
[Unit]
Description=ZeniFlow agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/zeniflow-agent run
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNITEOF
systemctl daemon-reload
systemctl enable --now zeniflow-agent
echo "Agent installed and started: systemctl status zeniflow-agent"
