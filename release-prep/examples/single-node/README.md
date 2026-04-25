# Single-node deployment example

The simplest possible MeshCDN setup: one VPS, one domain.

## Prerequisites

- A Linux VPS with public IP (Ubuntu 22.04+ recommended)
- A registered domain
- Telegram bot token + group ID (see [main deployment guide](../../docs/deployment.md))
- DNS A record pointing your domain at the VPS IP

## Step-by-step

### 1. Install MeshCDN

```bash
ssh root@your-vps
wget https://github.com/<org>/meshcdn/releases/latest/download/meshcdn-linux-amd64.tar.gz
tar xzf meshcdn-linux-amd64.tar.gz
sudo bash install.sh \
  --bot-token="<your_bot_token>" \
  --group-id="<your_group_id>"
```

Wait ~10 seconds. You should see `MeshCDN Agent ... 启动中...` and `OpenResty 已启动` in the logs:

```bash
sudo journalctl -u meshcdn -n 20 --no-pager
```

### 2. Verify in Telegram

Send `/menu` in your group. You should see the main menu with five categories.

### 3. Add your domain

Replace `example.com` and `1.2.3.4:443` with your domain and your origin (where the actual content is served from):

```
/w domain https://example.com 443 https://1.2.3.4:443
```

The bot replies confirming the domain is registered.

### 4. Get a TLS certificate

```
/w ssl example.com
```

The bot will issue a Let's Encrypt certificate. This takes ~10-30 seconds. You'll see a confirmation when done.

### 5. Test

```bash
curl -I https://example.com
```

You should get a `200 OK` (or whatever your origin returns), with the response served through MeshCDN.

### 6. (Optional) Add basic caching

```
/w cache example.com *.jpg,*.png,*.css,*.js 86400
```

This caches static assets for 1 day at the CDN layer.

## What you get

- Reverse proxy with auto-renewing SSL
- Configurable cache rules
- Defense rules (block IPs, rate limits, etc.)
- Telegram-based control plane
- Per-domain access logs and statistics

## Cost

Roughly:
- 1 VPS at $5/month (DigitalOcean, Vultr, Hetzner, etc.) = $5/month
- Domain registration ~ $10/year
- Total: under $7/month for a working CDN deployment

## When to scale to multi-node

Single node works fine for many small sites. Consider adding more nodes when:

- You need redundancy (single-node failure = downtime)
- Your traffic grows beyond what one VPS can handle
- You want geographic diversity (one node in EU, one in US, etc.)
- You want to test the mesh sync features

See [examples/multi-node](../multi-node/) for the multi-node setup.
