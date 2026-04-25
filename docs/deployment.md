# Deployment

This document covers deploying MeshCDN from scratch — single node, multi-node, with and without DNS integration.

For architectural background, see [architecture.md](architecture.md). For the command reference, see [configuration.md](configuration.md).

---

## System requirements

| | Minimum | Recommended |
|---|---|---|
| OS | Ubuntu 22.04 / Debian 12 | Ubuntu 22.04+ |
| Architecture | x86_64 | x86_64 |
| RAM | 512 MB | 1 GB |
| Disk | 1 GB | 5 GB (for cache + logs) |
| Open ports | 80, 443, 9443 | 80, 443, 9443 + custom |
| Outbound | HTTPS to `api.telegram.org` (Bot node only) | unrestricted |

Other distros (CentOS, Alpine, etc.) work if you build OpenResty from source. The default `install.sh` uses apt.

---

## Pre-flight: Telegram setup

You need a Telegram bot and a group before you start.

### 1. Create the bot

In Telegram, message [@BotFather](https://t.me/BotFather):

```
/newbot
```

Follow the prompts. Save the bot token — it looks like `1234567890:ABC...`. Keep it private; **anyone with this token can control your cluster**.

### 2. Configure the bot for group commands

Still in @BotFather, configure your bot:

```
/setprivacy → choose your bot → Disable
```

This lets the bot see all messages in the group (needed for `@botname question` AI queries). Alternative: keep privacy enabled, but make the bot a group admin (less broad permission).

### 3. Create the group

Create a Telegram group, add your bot. Then find the group ID:

- Add [@RawDataBot](https://t.me/raw_data_bot) to the group
- It will print the group's ID (a negative number like `-1001234567890`)
- Remove RawDataBot

Save the group ID. You'll use it as `--group-id`.

---

## Single-node deployment

The simplest deployment. Fully functional — you can add more nodes later.

```bash
# 1. Get the binary (or build from source — see below)
wget https://github.com/<org>/meshcdn/releases/latest/download/meshcdn-linux-amd64.tar.gz
tar xzf meshcdn-linux-amd64.tar.gz

# 2. Install
sudo bash install.sh \
  --bot-token="1234567890:ABC..." \
  --group-id="-1001234567890"

# 3. Verify
sudo systemctl status meshcdn
sudo journalctl -u meshcdn -n 20 --no-pager
```

You should see:
```
MeshCDN Agent v3.1.0-... 启动中...
OpenResty 已启动
公网 IP: x.x.x.x
Mesh 服务启动: :9443
Telegram Bot 已启动
```

In your Telegram group, send `/menu` — you should see the main menu.

---

## Multi-node deployment

The new node provisions itself by talking to an existing peer. No file uploads needed.

### Prerequisites

- One existing MeshCDN node running and reachable (the "introducer")
- The introducer's `:9443` port reachable from the new node
- Same bot token + group ID on every node

### Provision a new node

On the **new** machine:

```bash
curl -s http://<existing-node-ip>:9443/mesh/bootstrap | \
  sudo bash -s -- <existing-node-ip> "<bot-token>" <group-id>
```

This runs a script that:

1. Installs OpenResty if not already present
2. Downloads the `cdn-agent` binary from the introducer (sha256-verified)
3. Writes `/etc/meshcdn/config.json` with `peer_addr=<existing-node-ip>`
4. Creates the systemd unit
5. Starts the service

The new node then automatically:

1. Authenticates to the introducer using the shared secret derived from `(group_id, bot_token)`
2. Receives the current peer list and full config
3. Replays the config locally
4. Joins the mesh

Verify on the new node:

```bash
sudo journalctl -u meshcdn -n 30 --no-pager
```

You should see:
```
认亲成功! JoinOrder=N, 节点数=N+1
同步配置: <count> 条命令
Mesh 网络已启动 (节点数: N+1, Bot: <bot-node-id>)
```

In Telegram, `/v nodes` should now show all peers.

### Adding a third, fourth, ... node

Same command. The `--peer` argument can point to **any existing peer**, not necessarily the original Bot node:

```bash
# Either of these works equally well
curl -s http://node1:9443/mesh/bootstrap | sudo bash -s -- node1 "<token>" <group_id>
curl -s http://node2:9443/mesh/bootstrap | sudo bash -s -- node2 "<token>" <group_id>
```

The mesh design treats all peers equally.

---

## Building from source

If you'd rather build the binary yourself:

```bash
git clone https://github.com/<org>/meshcdn.git
cd meshcdn
go build -ldflags="-s -w" -o cdn-agent ./cmd/cdn-agent/
```

For a fully static binary (no glibc dependency, useful for older distros):

```bash
CGO_ENABLED=1 go build \
  -ldflags="-s -w -linkmode external -extldflags '-static'" \
  -o cdn-agent ./cmd/cdn-agent/
```

Then package it with `install.sh`:

```bash
tar czf meshcdn-linux-amd64.tar.gz cdn-agent scripts/install.sh
```

Go 1.21 or later required.

---

## Adding domains and certificates

Once the cluster is running, add a domain via Telegram:

```
# HTTPS domain pointing to backend at 1.2.3.4:443
/w domain https://example.com 443 https://1.2.3.4:443

# Same domain on multiple ports
/w domain https://example.com 443,8443 https://1.2.3.4:443

# Different origins for different ports
/w domain https://example.com 443 https://primary:443
/w domain https://example.com 8443 https://secondary:8443
```

For SSL certificates:

```
# Auto Let's Encrypt (default)
/w ssl example.com

# Specific node only
/w ssl example.com @<node-ip>

# Self-signed (for internal/testing)
/w ssl example.com selfsign
```

Certificate auto-renewal runs every 6 hours. You'll see notifications in the group when renewal happens.

---

## DNS setup

You need DNS A records pointing your domain at every node:

```
example.com → 1.2.3.4    (node A)
example.com → 5.6.7.8    (node B)
example.com → 9.10.11.12 (node C)
```

This gives you basic round-robin / multi-A-record load balancing. Most browsers and resolvers will retry on failure, so node failures degrade gracefully.

For automatic DNS removal of dead nodes (planned for v3.2), see [Roadmap](../README.md#roadmap).

### Working around DNS propagation

When you add a node, DNS needs to update before traffic flows there. To avoid downtime during propagation:

1. Configure the node, verify it works (test by setting your local `/etc/hosts` to point at the new node IP)
2. Add the new A record with a short TTL (60s)
3. Wait for propagation
4. Optionally raise TTL back to a normal value

---

## Verifying the deployment

A few sanity checks:

### Check OpenResty is listening on expected ports

```bash
sudo ss -lntp | grep openresty
# Should show 80, 443, plus any custom ports you've configured
```

### Check certificates

```bash
# Via Telegram
/v ssl
/v ssl health

# Via CLI
sudo cdn-agent exec "/v ssl"
```

### Check mesh connectivity

```bash
# From any node, list peers
sudo cdn-agent exec "/v nodes"

# Check the bot view
# In Telegram:
/v nodes
```

### Check end-to-end traffic

```bash
# Replace with your domain
curl -I https://example.com
```

You should get a `200` (or whatever your origin returns), with the response served by the node closest to your DNS resolver.

---

## Common issues

### "OpenResty 启动失败: nginx: [emerg] no ssl_certificate is defined"

Normal on first startup. The agent fails initial OpenResty start because no certs are present yet, then it issues an IP cert and retries. After a few seconds you'll see "OpenResty 已启动" and you're fine.

If this state persists for >1 minute, check:
```bash
sudo cat /etc/meshcdn/certs/<your-ip>.crt
# Should exist and be valid
```

### Bot is silent in Telegram

Most likely Privacy Mode. Either:
- Disable Privacy Mode in @BotFather: `/setprivacy → bot → Disable`, then **remove the bot from the group and re-add** (privacy changes don't propagate to existing memberships)
- Or make the bot a group admin (admin bots see all messages regardless of privacy mode)

Verify by checking logs:
```bash
sudo journalctl -u meshcdn --since "1 minute ago" --no-pager | grep "命令\|消息"
# If you see neither for messages you sent → Bot didn't receive them → privacy mode
```

### New node fails authentication

```
认亲失败: ...
```

Check:
- New node can reach `<introducer-ip>:9443` (try `curl http://<introducer>:9443/mesh/bootstrap`)
- Bot token and group ID match exactly between nodes
- No firewall blocking outbound traffic to port 9443

### Cert renewed but site serves old cert

This is the `renewed_not_live` state. Check:
```
/v ssl health
```

It will tell you which cert and what reload error caused the divergence. Most often it's an unrelated nginx config issue blocking reload entirely. Fix that, then run any `/w` command to trigger a reload, and the cert state will reconcile.

---

## Operating in restricted-network environments

If some nodes are in network environments that block outbound HTTPS to specific destinations (e.g. `api.telegram.org`), MeshCDN handles this automatically:

- Only the **Bot node** connects to Telegram
- Other nodes never even create the Telegram client object
- All inter-node communication is between peers on port 9443

So a typical deployment might be:

- 1 Bot node in an unrestricted region (US/EU)
- 2-3 worker nodes in regions where Telegram is blocked

The worker nodes serve traffic, hold certificates, and sync config — they just can't be the Bot node. That's fine.

If the Bot node fails, automatic drift moves the role to the next-available peer based on `join_order`. If your only unrestricted node fails, the surviving peers continue serving traffic but Telegram control is unavailable until you bring the Bot node back. (You can still use `cdn-agent exec` locally, and `cdn-agent takeover` to manually move the Bot role from the CLI.)

---

## Production checklist

Before pointing real traffic at a MeshCDN deployment:

- [ ] At least 2 nodes in the cluster (so single-node failure isn't an outage)
- [ ] DNS records for your domain point at all nodes with a sane TTL (300-3600s)
- [ ] Each node has working SSL certs (`/v ssl health` all green)
- [ ] Each node can reach all peers on port 9443
- [ ] Backups directory has free disk space (`/etc/meshcdn/backups/`)
- [ ] You've tested `cdn-agent restore --list` and know how to roll back
- [ ] The bot token is rotated from the development token (never use the same token in production as in testing)
- [ ] You've tested an upgrade on a non-critical node before applying to prod
- [ ] You've documented your bot token + group ID somewhere safe (you cannot recover them from a node — only from BotFather and Telegram)

---

## Uninstalling

To remove MeshCDN from a node while preserving OpenResty (which may be useful for the next install):

```bash
sudo systemctl stop meshcdn
sudo systemctl disable meshcdn
sudo rm -f /etc/systemd/system/meshcdn.service
sudo systemctl daemon-reload
sudo rm -f /usr/local/bin/cdn-agent
sudo rm -rf /etc/meshcdn
```

To remove everything including OpenResty:

```bash
# (above commands first), then:
sudo apt remove --purge -y openresty
sudo rm -rf /usr/local/openresty
```

---

## Where to read next

- [configuration.md](configuration.md) — full command reference
- [mesh-protocol.md](mesh-protocol.md) — what's happening over the wire
- [faq.md](faq.md) — common questions
