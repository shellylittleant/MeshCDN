#!/usr/bin/env bash
#
# MeshCDN — single-node upgrade helper.
#
# Downloads a release binary from GitHub, verifies it against the release's
# SHA256SUMS, installs it, and restarts the agent. Safe to re-run: the
# pre-upgrade backup is only written once, so a second run cannot clobber the
# rollback target with the binary you are rolling back *from*.
#
# Usage (on each node):
#
#   curl -fsSL https://raw.githubusercontent.com/shellylittleant/MeshCDN/main/scripts/upgrade-node.sh | sudo bash
#
#   # or pin a version:
#   curl -fsSL .../upgrade-node.sh | sudo MESHCDN_VERSION=v4.3.0 bash
#
# Why this exists rather than `cp`-ing the binary by hand: overwriting a running
# executable in place fails with ETXTBSY ("Text file busy"). The install here
# writes to <bin>.new and renames, which replaces the directory entry without
# touching the inode the kernel has mapped — the same thing the agent's own
# /mesh/upgrade path does.
#
# This script upgrades ONE node. For a cluster, either run it on every node, or
# run it on the bot node and then issue `/v upgrade - -` from Telegram to push
# the bot node's binary to the rest.

set -euo pipefail

VERSION="${MESHCDN_VERSION:-v4.3.0}"
REPO="${MESHCDN_REPO:-shellylittleant/MeshCDN}"
BIN="${MESHCDN_BIN:-/usr/local/bin/cdn-agent}"
SERVICE="meshcdn-agent.service"

TARBALL="meshcdn-${VERSION}-linux-amd64.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || fail "must run as root (try: curl -fsSL ... | sudo bash)"
[ -f "$BIN" ] || fail "$BIN not found — this looks like a fresh host; use install.sh instead"

CURRENT="$("$BIN" --version 2>/dev/null || echo 'unknown')"
say "current: $CURRENT"
say "target:  $VERSION"

# ── Download + verify ────────────────────────────────────────────────
cd "$WORKDIR"

say "downloading $TARBALL"
curl -fL --retry 3 --connect-timeout 15 -o "$TARBALL" "$BASE_URL/$TARBALL" \
  || fail "download failed — check that release $VERSION exists"

say "verifying checksum"
if curl -fsSL --retry 2 -o SHA256SUMS "$BASE_URL/SHA256SUMS" 2>/dev/null; then
    grep " $TARBALL\$" SHA256SUMS > expected.sha \
      || fail "SHA256SUMS has no entry for $TARBALL"
    sha256sum -c expected.sha || fail "CHECKSUM MISMATCH — refusing to install"
else
    fail "could not fetch SHA256SUMS from $BASE_URL — refusing to install unverified binary"
fi

tar xzf "$TARBALL" -C "$WORKDIR" 2>/dev/null
[ -f "$WORKDIR/cdn-agent" ] || fail "tarball did not contain cdn-agent"

# ── Install ──────────────────────────────────────────────────────────
# cp -n: never clobber an existing backup. Re-running this script must not
# overwrite the pre-upgrade binary with the post-upgrade one.
BACKUP="${BIN}.pre-${VERSION}"
if [ -f "$BACKUP" ]; then
    say "backup already present: $BACKUP (left untouched)"
else
    cp -n "$BIN" "$BACKUP"
    say "backed up current binary → $BACKUP"
fi

# Write-then-rename: an in-place overwrite of a running binary fails ETXTBSY.
cp "$WORKDIR/cdn-agent" "${BIN}.new"
chmod 755 "${BIN}.new"
mv -f "${BIN}.new" "$BIN"
say "binary replaced"

say "restarting $SERVICE"
systemctl restart "$SERVICE"

# ── Verify ───────────────────────────────────────────────────────────
sleep 3
NEW="$("$BIN" --version 2>/dev/null || echo 'unknown')"

if ! systemctl is-active --quiet "$SERVICE"; then
    printf '\033[1;31m\n'
    echo "agent did NOT come up. Rolling back."
    printf '\033[0m'
    mv -f "$BACKUP" "$BIN"
    systemctl restart "$SERVICE" || true
    echo
    echo "Rolled back to: $("$BIN" --version 2>/dev/null || echo unknown)"
    echo "Diagnose with:  journalctl -u $SERVICE -n 50 --no-pager"
    exit 1
fi

say "now running: $NEW"
say "service:     $(systemctl is-active "$SERVICE")"
echo
echo "Rollback if needed:"
echo "  mv -f $BACKUP $BIN && systemctl restart $SERVICE"
