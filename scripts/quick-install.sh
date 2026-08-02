#!/usr/bin/env bash
#
# MeshCDN — one-command install for a fresh node.
#
# Fetches a release from GitHub, verifies it against the release's SHA256SUMS,
# and hands off to the bundled installer. Picks the right one for you:
#
#   no --peer   → scripts/install.sh    (first node in a new cluster)
#   --peer=IP   → scripts/bootstrap.sh  (join an existing cluster)
#
# Usage — first node:
#
#   curl -fsSL https://raw.githubusercontent.com/shellylittleant/MeshCDN/main/scripts/quick-install.sh \
#     | sudo bash -s -- --bot-token=<TOKEN> --group-id=<GROUP_ID>
#
# Usage — join an existing cluster (point at ANY live peer):
#
#   curl -fsSL https://raw.githubusercontent.com/shellylittleant/MeshCDN/main/scripts/quick-install.sh \
#     | sudo bash -s -- --bot-token=<TOKEN> --group-id=<GROUP_ID> --peer=<ANY_LIVE_NODE_IP>
#
# Pin a version with MESHCDN_VERSION=v4.3.2; defaults to the latest release.
#
# This installs. To move an existing node to a newer build, use
# scripts/upgrade-node.sh instead — it backs up and rolls back, which an
# installer must not do.

set -euo pipefail

REPO="${MESHCDN_REPO:-shellylittleant/MeshCDN}"
VERSION="${MESHCDN_VERSION:-}"
BIN="/usr/local/bin/cdn-agent"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# ── Preconditions ────────────────────────────────────────────────────
[ "$(id -u)" -eq 0 ] || fail "must run as root (pipe into 'sudo bash', not plain 'bash')"

for tool in curl tar sha256sum; do
    command -v "$tool" >/dev/null 2>&1 || fail "missing required tool: $tool"
done

if [ -f "$BIN" ]; then
    printf '\033[1;33mWARNING:\033[0m %s already exists (%s).\n' \
        "$BIN" "$("$BIN" --version 2>/dev/null || echo 'version unknown')" >&2
    echo "This node looks installed already. To move it to a newer build run:" >&2
    echo "  curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/upgrade-node.sh | sudo bash" >&2
    fail "refusing to re-run the installer over an existing node"
fi

# ── Parse enough to choose an installer; everything passes through ──
PEER=""
HAVE_TOKEN="false"
HAVE_GROUP="false"
for arg in "$@"; do
    case "$arg" in
        --peer=*)      PEER="${arg#*=}" ;;
        --bot-token=*) HAVE_TOKEN="true" ;;
        --group-id=*)  HAVE_GROUP="true" ;;
    esac
done

[ "$HAVE_TOKEN" = "true" ] || fail "--bot-token=<TOKEN> is required"
[ "$HAVE_GROUP" = "true" ] || fail "--group-id=<GROUP_ID> is required"

# ── Resolve version ──────────────────────────────────────────────────
if [ -z "$VERSION" ]; then
    say "resolving latest release"
    VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
    [ -n "$VERSION" ] || fail "could not determine latest release; pass MESHCDN_VERSION=vX.Y.Z"
fi
say "version: $VERSION"

TARBALL="meshcdn-${VERSION}-linux-amd64.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"

# ── Download + verify ────────────────────────────────────────────────
cd "$WORKDIR"

say "downloading $TARBALL"
curl -fL --retry 3 --connect-timeout 15 -o "$TARBALL" "$BASE_URL/$TARBALL" \
    || fail "download failed — does release $VERSION publish a linux-amd64 build?"

say "verifying checksum"
curl -fsSL --retry 2 -o SHA256SUMS "$BASE_URL/SHA256SUMS" \
    || fail "could not fetch SHA256SUMS — refusing to install an unverified binary"
grep " $TARBALL\$" SHA256SUMS > expected.sha \
    || fail "SHA256SUMS has no entry for $TARBALL"
sha256sum -c expected.sha || fail "CHECKSUM MISMATCH — refusing to install"

mkdir -p pkg
tar xzf "$TARBALL" -C pkg 2>/dev/null
[ -x pkg/cdn-agent ] || fail "release tarball did not contain cdn-agent"

# install.sh looks for the binary next to itself; the tarball already lays it
# out that way, so just make sure the scripts are executable.
chmod +x pkg/install.sh pkg/bootstrap.sh 2>/dev/null || true

# ── Hand off ─────────────────────────────────────────────────────────
if [ -n "$PEER" ]; then
    [ -x pkg/bootstrap.sh ] || fail "release tarball did not contain bootstrap.sh"
    say "joining existing cluster via $PEER"
    exec bash pkg/bootstrap.sh "$@"
else
    [ -x pkg/install.sh ] || fail "release tarball did not contain install.sh"
    say "installing as the first node of a new cluster"
    say "(to join an existing cluster instead, re-run with --peer=<any live node IP>)"
    exec bash pkg/install.sh "$@"
fi
