#!/usr/bin/env bash
# MeshCDN install script — V4 step 3
#
# Changes from step 2:
#   - Now creates TWO systemd units:
#       meshcdn-nginx.service    runs OpenResty
#       meshcdn-agent.service    runs cdn-agent serve (renewal scanner/worker)
#   - Both auto-start on boot, agent depends on nginx
#   - lego dependency for ACME means we need go-sqlite3 + lego in the binary;
#     handled by go.mod when building, not by this script.

set -euo pipefail

# ─────────────────────────────────────────────────────────────────────
# Configuration
# ─────────────────────────────────────────────────────────────────────

MESHCDN_USER="root"
ETC_BASE="/etc/meshcdn"
PERSISTENT_DIR="$ETC_BASE/persistent"
RUNTIME_DIR="$ETC_BASE/runtime"
BIN_PATH="/usr/local/bin/cdn-agent"
SYSTEMD_NGINX="/etc/systemd/system/meshcdn-nginx.service"
SYSTEMD_AGENT="/etc/systemd/system/meshcdn-agent.service"

OPENRESTY_PATH="/usr/local/openresty"
OPENRESTY_NGINX="$OPENRESTY_PATH/nginx/sbin/nginx"

# ─────────────────────────────────────────────────────────────────────
# Argument parsing
# ─────────────────────────────────────────────────────────────────────

BOT_TOKEN=""
GROUP_ID=""
PEER_IP=""
SKIP_OPENRESTY="false"

for arg in "$@"; do
    case $arg in
        --bot-token=*)    BOT_TOKEN="${arg#*=}" ;;
        --group-id=*)     GROUP_ID="${arg#*=}" ;;
        --peer=*)         PEER_IP="${arg#*=}" ;;
        --skip-openresty) SKIP_OPENRESTY="true" ;;
        *)
            echo "Unknown argument: $arg" >&2
            echo "Usage: sudo bash install.sh --bot-token=<TOKEN> --group-id=<ID> [--peer=<IP>]" >&2
            exit 1
            ;;
    esac
done

if [ -z "$BOT_TOKEN" ] || [ -z "$GROUP_ID" ]; then
    echo "ERROR: --bot-token and --group-id are required" >&2
    exit 1
fi

if [ "$EUID" -ne 0 ]; then
    echo "ERROR: must run as root" >&2
    exit 1
fi

# ─────────────────────────────────────────────────────────────────────
# Detect OS
# ─────────────────────────────────────────────────────────────────────

detect_os() {
    if [ -f /etc/os-release ]; then
        # shellcheck disable=SC1091
        . /etc/os-release
        echo "$ID"
    else
        echo "unknown"
    fi
}

OS_ID=$(detect_os)
echo "Detected OS: $OS_ID"

# ─────────────────────────────────────────────────────────────────────
# Helpers
# ─────────────────────────────────────────────────────────────────────

probe_public_ip() {
    local ip=""
    for url in "https://api.ipify.org" "https://ifconfig.me/ip" "https://icanhazip.com"; do
        ip=$(curl -fsS --max-time 5 "$url" 2>/dev/null | tr -d '[:space:]' || true)
        if [[ "$ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
            echo "$ip"
            return 0
        fi
    done
    echo ""
    return 1
}

# ─────────────────────────────────────────────────────────────────────
# Step 1: Install OpenResty (if missing)
# ─────────────────────────────────────────────────────────────────────

install_openresty() {
    if [ -x "$OPENRESTY_NGINX" ]; then
        echo "✓ OpenResty already installed at $OPENRESTY_PATH"
        return
    fi

    echo "Installing OpenResty..."
    case "$OS_ID" in
        ubuntu|debian)
            apt-get update -qq
            apt-get install -y -qq curl gnupg ca-certificates lsb-release
            mkdir -p /etc/apt/keyrings

            # Pick the fastest mirror for OpenResty. Official is openresty.org;
            # tuna (清华) mirrors it for users in China where openresty.org is slow.
            # Try official first with short timeout; fall back to tuna if it fails.
            OR_KEY_URL=""
            OR_REPO_BASE=""
            for cand_key in \
                "https://openresty.org/package/pubkey.gpg" \
                "https://mirrors.tuna.tsinghua.edu.cn/openresty/pubkey.gpg" ; do
                if curl -fsSL --connect-timeout 8 --max-time 30 -o /tmp/openresty-pubkey.tmp "$cand_key" 2>/dev/null; then
                    OR_KEY_URL="$cand_key"
                    if [ "$cand_key" = "https://openresty.org/package/pubkey.gpg" ]; then
                        OR_REPO_BASE="http://openresty.org/package"
                    else
                        OR_REPO_BASE="https://mirrors.tuna.tsinghua.edu.cn/openresty"
                    fi
                    echo "  using OpenResty mirror: $OR_REPO_BASE"
                    break
                fi
                echo "  ✗ $cand_key unreachable, trying next mirror"
            done

            if [ -z "$OR_KEY_URL" ]; then
                echo "ERROR: cannot reach any OpenResty mirror." >&2
                echo "       Check outbound HTTPS, then re-run." >&2
                exit 1
            fi

            # Convert ASCII-armored key to binary keyring
            if ! gpg --dearmor -o /etc/apt/keyrings/openresty.gpg --yes /tmp/openresty-pubkey.tmp; then
                echo "ERROR: gpg --dearmor failed" >&2
                exit 1
            fi
            rm -f /tmp/openresty-pubkey.tmp

            CODENAME=$(lsb_release -sc 2>/dev/null || echo "jammy")
            # Ubuntu uses path "ubuntu" + component "main"; Debian uses "debian" + "openresty"
            if [ "$OS_ID" = "debian" ]; then
                REPO_LINE="deb [signed-by=/etc/apt/keyrings/openresty.gpg] $OR_REPO_BASE/debian $CODENAME openresty"
            else
                REPO_LINE="deb [signed-by=/etc/apt/keyrings/openresty.gpg] $OR_REPO_BASE/ubuntu $CODENAME main"
            fi
            echo "$REPO_LINE" > /etc/apt/sources.list.d/openresty.list

            apt-get update -qq
            apt-get install -y openresty
            ;;
        centos|rhel|fedora|rocky|almalinux)
            yum install -y yum-utils
            yum-config-manager --add-repo https://openresty.org/package/centos/openresty.repo
            yum install -y openresty
            ;;
        *)
            echo "ERROR: unsupported OS '$OS_ID'. Install OpenResty manually." >&2
            echo "       Then re-run with --skip-openresty." >&2
            exit 1
            ;;
    esac

    if [ ! -x "$OPENRESTY_NGINX" ]; then
        echo "ERROR: OpenResty install completed but binary not found at $OPENRESTY_NGINX" >&2
        exit 1
    fi

    # OpenResty's deb/rpm post-install enables and starts the openresty.service
    # systemd unit, which would race with our meshcdn-nginx.service for port 80/443.
    # Mask it permanently — we manage nginx ourselves through meshcdn-nginx.
    systemctl stop openresty.service 2>/dev/null || true
    systemctl disable openresty.service 2>/dev/null || true
    systemctl mask openresty.service 2>/dev/null || true

    echo "✓ OpenResty installed (default service masked)"
}

if [ "$SKIP_OPENRESTY" = "false" ]; then
    install_openresty
else
    echo "⚠ Skipping OpenResty install (--skip-openresty)"
fi

# ─────────────────────────────────────────────────────────────────────
# Step 2: Create directory structure
# ─────────────────────────────────────────────────────────────────────

echo "Creating directory structure under $ETC_BASE/ ..."

mkdir -p "$PERSISTENT_DIR/certs"
mkdir -p "$RUNTIME_DIR/nginx/servers"
mkdir -p "$RUNTIME_DIR/welcome"
mkdir -p "$RUNTIME_DIR/challenges/.well-known/acme-challenge"
mkdir -p "$RUNTIME_DIR/cache"
mkdir -p "$RUNTIME_DIR/logs"
mkdir -p "$RUNTIME_DIR/tmp"

chmod 700 "$PERSISTENT_DIR"
chmod 700 "$PERSISTENT_DIR/certs"
chmod 755 "$RUNTIME_DIR"
chmod 755 "$RUNTIME_DIR/challenges"
chmod 755 "$RUNTIME_DIR/challenges/.well-known"
chmod 755 "$RUNTIME_DIR/challenges/.well-known/acme-challenge"

echo "✓ Directories created"

# ─────────────────────────────────────────────────────────────────────
# Step 3: Probe public IP
# ─────────────────────────────────────────────────────────────────────

NODE_IP="${NODE_IP:-}"
if [ -z "$NODE_IP" ]; then
    NODE_IP=$(probe_public_ip)
fi
if [ -z "$NODE_IP" ]; then
    echo "ERROR: could not probe public IP. Specify with: NODE_IP=x.x.x.x bash install.sh ..." >&2
    exit 1
fi
echo "✓ Public IP: $NODE_IP"

# ─────────────────────────────────────────────────────────────────────
# Step 4: Install cdn-agent binary
# ─────────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CDN_AGENT_SRC=""
for path in "$SCRIPT_DIR/cdn-agent" "$PWD/cdn-agent" "/tmp/cdn-agent"; do
    if [ -x "$path" ]; then
        CDN_AGENT_SRC="$path"
        break
    fi
done

if [ -z "$CDN_AGENT_SRC" ]; then
    echo "ERROR: cdn-agent binary not found alongside install.sh." >&2
    exit 1
fi

# Stop existing services first — the running binary holds the file open
# ("Text file busy" otherwise). Idempotent: ok if services don't exist yet.
systemctl stop meshcdn-agent.service 2>/dev/null || true
systemctl stop meshcdn-nginx.service 2>/dev/null || true

cp "$CDN_AGENT_SRC" "$BIN_PATH"
chmod 755 "$BIN_PATH"
echo "✓ cdn-agent installed to $BIN_PATH"

# ─────────────────────────────────────────────────────────────────────
# Step 5: Generate self-signed cert
# ─────────────────────────────────────────────────────────────────────

echo "Generating self-signed cert for $NODE_IP ..."
"$BIN_PATH" install-bootstrap \
    --node-ip="$NODE_IP" \
    --certs-dir="$PERSISTENT_DIR/certs"
echo "✓ Self-signed certificate generated"

# ─────────────────────────────────────────────────────────────────────
# Step 6: Write identity.json
# ─────────────────────────────────────────────────────────────────────

cat > "$PERSISTENT_DIR/identity.json" <<EOF
{
  "node_ip":   "$NODE_IP",
  "bot_token": "$BOT_TOKEN",
  "group_id":  $GROUP_ID,
  "joined_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF
chmod 600 "$PERSISTENT_DIR/identity.json"
echo "✓ identity.json written"

# ─────────────────────────────────────────────────────────────────────
# Step 7: Initialize peers.json
# ─────────────────────────────────────────────────────────────────────

if [ ! -f "$PERSISTENT_DIR/peers.json" ]; then
    cat > "$PERSISTENT_DIR/peers.json" <<EOF
{
  "peers": [
    { "ip": "$NODE_IP", "join_order": 1 }
  ]
}
EOF
    chmod 600 "$PERSISTENT_DIR/peers.json"
    echo "✓ peers.json initialized"
fi

# ─────────────────────────────────────────────────────────────────────
# Step 8: Welcome page
# ─────────────────────────────────────────────────────────────────────

cat > "$RUNTIME_DIR/welcome/index.html" <<EOF
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>MeshCDN Node</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, sans-serif;
           max-width: 600px; margin: 5em auto; padding: 2em; color: #333; }
    h1 { color: #555; }
    code { background: #f0f0f0; padding: 2px 6px; border-radius: 3px; }
    .footer { margin-top: 3em; color: #999; font-size: 0.9em; }
  </style>
</head>
<body>
  <h1>MeshCDN Node</h1>
  <p>This is a MeshCDN node. There's no application configured for the requested host.</p>
  <p>If you're an operator:</p>
  <p><code>cdn-agent exec "/w domain https://your.domain:443 https://origin:443"</code></p>
  <p><code>cdn-agent exec "/w ssl your.domain -"</code></p>
  <div class="footer">MeshCDN — self-hosted distributed CDN</div>
</body>
</html>
EOF
echo "✓ Welcome page created"

# ─────────────────────────────────────────────────────────────────────
# Step 9: Initial nginx config
# ─────────────────────────────────────────────────────────────────────

echo "Generating initial nginx config ..."
"$BIN_PATH" install-bootstrap --regen-nginx --node-ip="$NODE_IP" \
    --certs-dir="$PERSISTENT_DIR/certs" \
    --nginx-dir="$RUNTIME_DIR/nginx" \
    --db-path="$RUNTIME_DIR/config.db" \
    --welcome-dir="$RUNTIME_DIR/welcome"
echo "✓ Initial nginx config generated"

# ─────────────────────────────────────────────────────────────────────
# Step 10: systemd units (nginx + agent)
# ─────────────────────────────────────────────────────────────────────

cat > "$SYSTEMD_NGINX" <<EOF
[Unit]
Description=MeshCDN OpenResty
After=network.target

[Service]
Type=simple
User=$MESHCDN_USER
ExecStartPre=/bin/bash -c '$OPENRESTY_NGINX -t -c $RUNTIME_DIR/nginx/nginx.conf'
ExecStart=$OPENRESTY_NGINX -c $RUNTIME_DIR/nginx/nginx.conf -g 'daemon off;'
ExecReload=$OPENRESTY_NGINX -s reload -c $RUNTIME_DIR/nginx/nginx.conf
KillMode=mixed
TimeoutStopSec=5
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF

cat > "$SYSTEMD_AGENT" <<EOF
[Unit]
Description=MeshCDN Agent (renewal scanner + worker)
After=meshcdn-nginx.service
Requires=meshcdn-nginx.service

[Service]
Type=simple
User=$MESHCDN_USER
ExecStart=$BIN_PATH serve
KillMode=mixed
TimeoutStopSec=10
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable meshcdn-nginx.service
systemctl enable meshcdn-agent.service
systemctl start meshcdn-nginx.service
systemctl start meshcdn-agent.service

sleep 2
if ! systemctl is-active --quiet meshcdn-nginx.service; then
    echo "⚠ meshcdn-nginx failed to start. Check: journalctl -u meshcdn-nginx -n 50" >&2
    exit 1
fi
echo "✓ meshcdn-nginx started"

if ! systemctl is-active --quiet meshcdn-agent.service; then
    echo "⚠ meshcdn-agent failed to start. Check: journalctl -u meshcdn-agent -n 50" >&2
    exit 1
fi
echo "✓ meshcdn-agent started"

# ─────────────────────────────────────────────────────────────────────
# Step 11: Final summary
# ─────────────────────────────────────────────────────────────────────

cat <<EOF

══════════════════════════════════════════════════════════════════════
  ✅ MeshCDN installed successfully
══════════════════════════════════════════════════════════════════════

  Node IP:        $NODE_IP
  Group ID:       $GROUP_ID
  Identity file:  $PERSISTENT_DIR/identity.json
  Services:
    - meshcdn-nginx (OpenResty)
    - meshcdn-agent (renewal scanner/worker)

  Next steps:

    1. Add a domain (must already point at this node via DNS):
       sudo cdn-agent exec "/w domain https://your-domain.com:443 https://your-origin:443"

    2. Issue a Let's Encrypt cert:
       sudo cdn-agent exec "/w ssl your-domain.com -"

    3. Verify:
       curl https://your-domain.com/

  Logs:
    journalctl -u meshcdn-nginx -f
    journalctl -u meshcdn-agent -f

══════════════════════════════════════════════════════════════════════
EOF
