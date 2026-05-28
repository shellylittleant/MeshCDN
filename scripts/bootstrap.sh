#!/usr/bin/env bash
# MeshCDN bootstrap — join an existing cluster from a fresh VPS.
#
# Per V4-DESIGN §5.1:
#   1. Download cdn-agent binary from an existing peer (mesh download endpoint
#      to be added in step 5; for now we expect bootstrap.tar.gz alongside
#      this script, same as install.sh)
#   2. Run install.sh as usual to create persistent/identity.json + self-signed cert
#   3. Make /mesh/auth call to introducer with our (group_id, bot_token) secret
#   4. Receive peer list + snapshot
#   5. Replay snapshot locally, write peers.json, restart agent
#
# Usage (run on the NEW node):
#   sudo bash bootstrap.sh \
#       --bot-token=<TOKEN> --group-id=<ID> --peer=<INTRODUCER_IP>
#
# This script assumes:
#   - Same TLS cert format as the introducer's /mesh/auth endpoint expects
#   - The introducer is already running V4 with mesh enabled
#   - cdn-agent + install.sh are present in the same directory as this script

set -euo pipefail

# ─────────────────────────────────────────────────────────────────────
# Argument parsing
# ─────────────────────────────────────────────────────────────────────

BOT_TOKEN=""
GROUP_ID=""
PEER_IP=""
MESH_PORT="9443"

for arg in "$@"; do
    case $arg in
        --bot-token=*) BOT_TOKEN="${arg#*=}" ;;
        --group-id=*)  GROUP_ID="${arg#*=}" ;;
        --peer=*)      PEER_IP="${arg#*=}" ;;
        --mesh-port=*) MESH_PORT="${arg#*=}" ;;
        *)
            echo "Unknown argument: $arg" >&2
            echo "Usage: sudo bash bootstrap.sh --bot-token=<TOKEN> --group-id=<ID> --peer=<IP>" >&2
            exit 1
            ;;
    esac
done

if [ -z "$BOT_TOKEN" ] || [ -z "$GROUP_ID" ] || [ -z "$PEER_IP" ]; then
    echo "ERROR: --bot-token, --group-id, and --peer are all required" >&2
    exit 1
fi

if [ "$EUID" -ne 0 ]; then
    echo "ERROR: must run as root" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ─────────────────────────────────────────────────────────────────────
# Step 0: Ensure jq is installed (we need it to parse the handshake response)
# ─────────────────────────────────────────────────────────────────────

if ! command -v jq >/dev/null 2>&1; then
    echo "→ Installing jq ..."
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update -qq && apt-get install -y -qq jq
    elif command -v yum >/dev/null 2>&1; then
        yum install -y -q jq
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y -q jq
    else
        echo "ERROR: please install 'jq' manually and re-run" >&2
        exit 1
    fi
fi

# ─────────────────────────────────────────────────────────────────────
# Step 1: Standard install (creates identity.json, self-signed cert, etc.)
# ─────────────────────────────────────────────────────────────────────

if [ ! -x "$SCRIPT_DIR/install.sh" ]; then
    echo "ERROR: install.sh not found alongside bootstrap.sh" >&2
    exit 1
fi

# Run install.sh first to set up the node identity locally.
# install.sh handles OpenResty install, identity.json, self-signed, etc.
echo "→ Running install.sh ..."
bash "$SCRIPT_DIR/install.sh" \
    --bot-token="$BOT_TOKEN" \
    --group-id="$GROUP_ID"

# ─────────────────────────────────────────────────────────────────────
# Step 2: Compute auth token (sha256(group_id + bot_token))
# ─────────────────────────────────────────────────────────────────────

# Same derivation as identity.Secret() in Go.
TOKEN=$(printf "%s%s" "$GROUP_ID" "$BOT_TOKEN" | sha256sum | awk '{print $1}')

# Probe our own public IP for the handshake request.
# (install.sh has already detected this; re-detect here for safety.)
NODE_IP=$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null | tr -d '[:space:]' || true)
if [ -z "$NODE_IP" ]; then
    # Fall back to identity.json
    NODE_IP=$(grep -o '"node_ip":[[:space:]]*"[^"]*"' /etc/meshcdn/persistent/identity.json | \
              sed 's/.*"\([^"]*\)"/\1/')
fi

if [ -z "$NODE_IP" ]; then
    echo "ERROR: could not determine our node IP" >&2
    exit 1
fi

echo "→ Our node IP: $NODE_IP"
echo "→ Introducer:  $PEER_IP:$MESH_PORT"

# ─────────────────────────────────────────────────────────────────────
# Step 3: Stop agent so we can manipulate state safely
# ─────────────────────────────────────────────────────────────────────

systemctl stop meshcdn-agent.service 2>/dev/null || true

# ─────────────────────────────────────────────────────────────────────
# Step 4: Call /mesh/auth on the introducer
# ─────────────────────────────────────────────────────────────────────

echo "→ Authenticating to introducer ..."

REQUEST=$(printf '{"node_ip":"%s"}' "$NODE_IP")
RESPONSE_FILE=$(mktemp)
trap "rm -f $RESPONSE_FILE" EXIT

HTTP_CODE=$(curl -sk -X POST \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $TOKEN" \
    -d "$REQUEST" \
    -o "$RESPONSE_FILE" \
    -w "%{http_code}" \
    --max-time 30 \
    "https://${PEER_IP}:${MESH_PORT}/mesh/auth")

if [ "$HTTP_CODE" != "200" ]; then
    echo "ERROR: handshake failed (HTTP $HTTP_CODE)" >&2
    cat "$RESPONSE_FILE" >&2
    exit 1
fi

# ─────────────────────────────────────────────────────────────────────
# Step 5: Parse response, extract snapshot, peers, join_order
# ─────────────────────────────────────────────────────────────────────

OK=$(jq -r '.ok' "$RESPONSE_FILE")
if [ "$OK" != "true" ]; then
    REASON=$(jq -r '.reason // "unknown"' "$RESPONSE_FILE")
    echo "ERROR: introducer rejected: $REASON" >&2
    exit 1
fi
JOIN_ORDER=$(jq -r '.join_order' "$RESPONSE_FILE")
SNAPSHOT=$(jq -r '.snapshot' "$RESPONSE_FILE")
PEERS_JSON=$(jq -c '{peers: .peers}' "$RESPONSE_FILE")

echo "→ Joined as join_order=$JOIN_ORDER"

# ─────────────────────────────────────────────────────────────────────
# Step 6: Write peers.json + apply snapshot
# ─────────────────────────────────────────────────────────────────────

echo "$PEERS_JSON" > /etc/meshcdn/persistent/peers.json
chmod 600 /etc/meshcdn/persistent/peers.json
echo "→ peers.json written"

# Save snapshot to a temp file and use the agent to import it
# (we use the same code path as live mesh pull, for consistency)
SNAPSHOT_TMP=$(mktemp)
echo "$SNAPSHOT" > "$SNAPSHOT_TMP"

# Move to persistent location (will be re-read on startup)
mv "$SNAPSHOT_TMP" /etc/meshcdn/persistent/snapshot.cmd
chmod 600 /etc/meshcdn/persistent/snapshot.cmd
echo "→ snapshot.cmd written"

# Force a re-import: delete config.db so agent rebuilds from snapshot
rm -f /etc/meshcdn/runtime/config.db

# ─────────────────────────────────────────────────────────────────────
# Step 7: Auto-disable bot on this worker node
# ─────────────────────────────────────────────────────────────────────
#
# Only one node in the cluster can poll Telegram (multiple nodes would
# fight over getUpdates and get HTTP 409 responses). The introducer (peer
# we just joined) is the bot node by default. We're a new worker — disable
# bot here.
#
# Per V4-DESIGN: alerts from worker nodes are forwarded to the bot node
# via mesh /mesh/event. So Telegram still sees alerts from us, just routed.

mkdir -p /etc/systemd/system/meshcdn-agent.service.d
cat > /etc/systemd/system/meshcdn-agent.service.d/worker.conf <<EOF
# Auto-generated by bootstrap.sh — this is a worker node, not the bot node.
# The bot polls Telegram on the introducer (smallest join_order). We forward
# our alerts there via mesh.
[Service]
Environment="MESHCDN_BOT_DISABLE=1"
EOF
systemctl daemon-reload
echo "→ bot polling disabled on this worker node (alerts forwarded via mesh)"

# ─────────────────────────────────────────────────────────────────────
# Step 8: Restart agent
# ─────────────────────────────────────────────────────────────────────

systemctl restart meshcdn-nginx.service
systemctl restart meshcdn-agent.service

sleep 3

if ! systemctl is-active --quiet meshcdn-agent.service; then
    echo "ERROR: agent failed to start after bootstrap" >&2
    journalctl -u meshcdn-agent -n 30 --no-pager >&2
    exit 1
fi

# ─────────────────────────────────────────────────────────────────────
# Final summary
# ─────────────────────────────────────────────────────────────────────

cat <<EOF

══════════════════════════════════════════════════════════════════════
  ✅ Successfully joined cluster
══════════════════════════════════════════════════════════════════════

  Our node IP:    $NODE_IP
  Join order:     $JOIN_ORDER
  Introducer:     $PEER_IP

  Verify:
    cdn-agent exec "/v domain - -"
    journalctl -u meshcdn-agent -f

══════════════════════════════════════════════════════════════════════
EOF
