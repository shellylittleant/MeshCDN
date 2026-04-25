# FAQ

Common questions about MeshCDN.

If your question isn't here, [open an issue](../../issues/new) — and we'll add it.

---

## General

### Is MeshCDN production-ready?

**Alpha-grade.** It's been running real production traffic on the v2.x line for over a year. The v3.x line adds significant features (rule templates, smart routing, certificate hardening) and is currently at `v3.1.0-alpha11`. We expect to mark a stable v3.1.0 release after another few weeks of testing.

For non-critical use, it's usable today. For mission-critical traffic, wait for v3.1.0 stable, or run v2.11.x (the previous stable line).

### How does MeshCDN compare to Cloudflare?

Different scales of problem.

Cloudflare runs ~300 PoPs globally and serves a non-trivial fraction of internet traffic. MeshCDN can't replicate that — you'd need millions of dollars of infrastructure.

But for use cases that don't need 300 PoPs:
- A few strategic VPS instances ($5-20/month each)
- Self-controlled, no third-party dependency
- Custom defense rules in Lua
- No vendor lock-in

…MeshCDN is a viable alternative. It's "self-hosted CDN for organizations that don't need (or can't afford) commercial CDN services."

### Why Telegram for the control plane? That seems weird.

It is weird, but it works really well. Long answer in [architecture.md § Why Telegram as the control plane](architecture.md#why-telegram-as-the-control-plane). Short answer:

- Free
- Mobile-first (operate from a phone in any cafe)
- Built-in audit log (group history)
- Built-in notification channel
- No public-facing admin port to defend
- Multi-user, multi-device by default

The architecture supports replacing Telegram with another frontend (the command language is the product), but Telegram-only is what's shipped today.

### Can I use this without Telegram?

Yes. Every command works via the local CLI:

```bash
sudo cdn-agent exec "/w domain https://example.com 443 https://1.2.3.4:443"
```

You'd lose:
- The mobile UX
- Cluster-wide audit log
- Telegram notifications

But the system functions. You'll need SSH access to operate it. This setup is more like "self-hosted CDN with familiar Unix CLI" than "self-hosted CDN with chat UX."

A web UI is on the longer-term roadmap. Contributions welcome.

### Does it really need OpenResty? Can I use plain nginx?

OpenResty is nginx + LuaJIT + a few modules. We use Lua for the more sophisticated defense rules (CC protection, GeoIP filtering). Without OpenResty:

- All the basic nginx features still work (deny/allow, rate limiting, proxy_pass, SSL)
- The Lua-based rules don't (currently planned: `geo:`, `ua:`, `cc:` prefixes in `/w defense`)

If you're OK losing those features and want to use plain nginx, you could fork the project and replace the OpenResty dependency. The vast majority of code wouldn't need to change.

---

## Networking

### Does it work behind NAT?

Inbound: needs a public IP for the CDN ports (80, 443, custom) — that's by definition. NAT'd nodes can't serve external traffic.

Outbound: works fine. Mesh traffic (port 9443) is between the public IPs of nodes.

### What if some nodes can't reach `api.telegram.org`?

Fine. Only the Bot node connects to Telegram. Other nodes stay completely disconnected from Telegram. See [deployment.md § Operating in restricted-network environments](deployment.md#operating-in-restricted-network-environments).

If your only unrestricted node fails, the surviving nodes continue serving traffic but Telegram control is unavailable until you bring an unrestricted node back.

### How much bandwidth does mesh communication use?

Negligible:
- Heartbeats: ~500 bytes per node-pair per minute
- Broadcasts: ~200-1000 bytes per command, broadcast to all peers
- Cert syncs: ~5-10 KB per certificate, only when renewing
- Periodic reconciliations: only when versions diverge (rare in normal operation)

For a 10-node cluster doing typical config changes, expect <1 MB/day of mesh traffic.

### Can nodes be in different cloud providers?

Yes — this is one of the design goals. As long as they have public IPs and can reach each other on port 9443, they can be on AWS, GCP, Hetzner, DigitalOcean, Vultr, Aliyun, whatever. The mesh doesn't care.

We've tested across providers including DigitalOcean, Vultr, and Aliyun. No provider-specific issues.

---

## SSL & Certificates

### What's the deal with IP certificates vs domain certificates?

- **Domain certificates** are for your actual websites. Issued by Let's Encrypt. Synced cluster-wide.
- **IP certificates** are for each node's own IP. Used for mesh communication (so peers can verify they're talking to the right node) and as a fallback "default server" cert. **Not** synced — each node has its own.

Domain certs are what you use for SSL on your sites. IP certs are infrastructure.

### Why does Let's Encrypt fail for my IP cert?

Let's Encrypt doesn't issue certs for IP addresses (only domains). MeshCDN tries LE first because LE works for the rare case where you set up a reverse DNS entry pointing your IP at a name LE recognizes. In the much more common case where this fails, MeshCDN falls back to a self-signed cert, which is fine for the IP-cert use case.

You'll see this in the logs:
```
LE 申请失败: 验证失败: 域名 1.2.3.4 无法通过 HTTP-01 验证
✅ v3.0: IP 证书已写入 /etc/meshcdn/certs/1.2.3.4.crt
```

This is normal.

### What's `renewed_not_live` mean and why should I care?

It's the most operationally important certificate state. It means: "the new cert is on disk, but nginx couldn't reload to pick it up."

Without this state, you'd have a silent failure: renewals appear successful, but nginx is still serving the old cert. When the old cert eventually expires, your site goes down — and you'd have no warning.

With this state, you get a Telegram alert immediately when the divergence happens. `/v ssl health` shows a 🚨 next to affected certs. You can investigate and fix the underlying nginx error before the old cert expires.

### Can I use my own certificates (not LE)?

Currently: yes, but not via the Telegram UI. You'd need to:

1. Stop the agent
2. Drop your `cert.crt` and `cert.key` into `/etc/meshcdn/certs/` named after the domain
3. Add an entry to `manifest.json` (see [architecture.md](architecture.md))
4. Restart the agent

A `/w ssl` subcommand for "upload custom cert" is planned. Contributions welcome.

---

## Operation

### What if my Bot node goes down?

After 3 missed heartbeats (~3 minutes), automatic drift moves the Bot role to the next-available peer (smallest `join_order` among survivors). You'll see this in the group:

```
Bot 已切换至节点 <peer>
```

Telegram control resumes immediately. Manual recovery: bring the original node back online; you can move the Bot role back with `/target <ip>` if desired.

### What if I lose access to my Telegram bot token?

You're locked out of Telegram control, but the cluster keeps running. To recover:

1. Create a new bot via @BotFather
2. SSH into any node, edit `/etc/meshcdn/config.json` with the new token
3. Restart: `sudo systemctl restart meshcdn`
4. Repeat for every node (this is one of the few cases where you have to touch every node)

The cluster keeps serving traffic the whole time — no downtime.

### How do I roll back a bad upgrade?

If you upgraded recently:

```bash
sudo cdn-agent restore --list
sudo systemctl stop meshcdn
sudo cdn-agent restore --backup=<timestamp>
sudo systemctl start meshcdn
```

Pre-upgrade backups are automatic (since v3.1.0-alpha8). The backup contains `config.db`, `manifest.json`, `peers.json`. Cert files and `config.json` aren't in the backup because they're never deleted.

If you can't find a recent backup but you have other healthy peers in the cluster:
1. Stop the broken node
2. Delete its `/etc/meshcdn/config.db`
3. Restart it — it'll re-authenticate and pull config from peers via mesh sync.

### Can I have multiple clusters?

Yes — each cluster is identified by its `(bot_token, group_id)` tuple. Use a different bot+group for each cluster. Nodes from cluster A can't authenticate to cluster B.

Multi-tenant on shared hardware (multiple clusters on one VPS) isn't supported today.

### How do I migrate from a different CDN?

There's no automatic import. The migration path:

1. Set up the MeshCDN cluster
2. Use `/w domain` to register all domains (origin pointing at your existing CDN, if you want zero-downtime)
3. Issue certs (`/w ssl`)
4. Test by setting your local `/etc/hosts` to point your domain at MeshCDN nodes
5. When ready, change DNS to point at MeshCDN nodes
6. After DNS propagates, point origins at the actual backend servers (not the old CDN)
7. Decommission the old CDN

Or if downtime is acceptable: cut DNS over directly.

---

## AI

### What does the AI actually do?

It can answer questions like:
- "Which certificates are expiring within 30 days?"
- "What cache rules are active for example.com?"
- "Show me the access traffic for the last 24 hours"
- "Are any nodes currently failing?"

It does this by:
1. Receiving your natural-language question
2. Translating it to SQL queries against the cluster's read-only DB views
3. Executing those queries
4. Composing a natural-language answer from the results

It does **not** execute any `/w` or `/d` commands. Read-only by design.

### Why no auto-fix?

Configuration changes from a non-deterministic system (LLM) running unattended on production infrastructure is a great way to have a really bad day. We deliberately limit AI to advisory mode.

If you ask "should I increase the cache TTL on a.com?", the AI will give a recommendation (e.g. "Currently 60 seconds, very short for static images. Suggest 604800 (1 week)."). You then decide and run `/w cache a.com *.jpg 604800` yourself. Always.

### Can I use a different LLM provider?

DeepSeek and OpenAI-compatible APIs are supported out of the box. You can point at any OpenAI-API-compatible endpoint (Together, Anthropic via API gateway, local llama.cpp servers, etc.) by setting the right base URL.

### What about privacy / data sent to LLM?

When you ask the AI a question, the question + relevant DB query results are sent to the configured LLM provider. This includes domain names, cert metadata, traffic stats, etc.

If your config DB contains sensitive info (e.g. internal hostnames you don't want to share with OpenAI), you have two options:

1. Don't ask AI questions about sensitive parts of the config
2. Self-host an LLM (e.g. a Llama 3 deployment) and point `ai_*_key` at it

The AI feature is fully optional — leaving `ai_provider` unset disables it.

---

## Contributing

### I found a bug. How do I report it?

See [CONTRIBUTING.md](../CONTRIBUTING.md). Short version: open a GitHub issue with version, cluster size, exact command, expected vs actual behavior, and logs. Redact your bot token before pasting logs.

### I want to add a feature. How do I propose it?

Open an issue first to discuss. We'd rather have a 5-minute conversation up front than have you spend a weekend on a PR that doesn't fit the project direction.

### What's the test situation?

Sparse, honestly. Manual testing on a 1-2 node cluster is the current bar. Adding a real test suite is on the v3.2 roadmap.

### Can I run this for my own use without contributing back?

Yes. Apache 2.0 license. Use it for anything, including commercial purposes. We'd love to hear about your deployment, but you're not obligated to share.

---

## Misc

### Why "MeshCDN"?

"Mesh" because of the peer-to-peer architecture (every node talks to every other node, no master). "CDN" because that's the use case. Working name; it stuck.

### Can I run this on Raspberry Pi / ARM?

The code is portable Go. The default `install.sh` and release binaries are amd64. To run on ARM:

1. Build the binary: `GOOS=linux GOARCH=arm64 go build ./cmd/cdn-agent/`
2. Make sure OpenResty is installed for ARM
3. Adjust install.sh as needed

We haven't extensively tested on ARM, but there's no architectural reason it wouldn't work.

### What's the disk usage like?

Per node:
- Binary: ~5 MB
- Logs (rotated daily, kept 7 days): typically <100 MB
- Cache (configurable, default 1 GB): up to your `proxy_cache_path` setting
- DB: typically <10 MB unless you have hundreds of domains

A 1 GB disk is fine for small deployments. 5+ GB recommended if you cache aggressively.

### What's the memory usage like?

`cdn-agent` itself: ~30-50 MB resident.
OpenResty: depends on traffic and cache, typically 50-200 MB.

A 512 MB VPS works for low-traffic deployments. 1 GB is comfortable.

### Is there a Docker image?

Not officially yet. The agent + OpenResty in one container is a sensible package. Contributions welcome.
