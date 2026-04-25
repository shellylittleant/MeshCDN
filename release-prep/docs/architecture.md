# Architecture

This document explains MeshCDN's design — what we built, why we built it that way, and how the pieces fit together.

If you just want to deploy MeshCDN, skip to [deployment.md](deployment.md). This document is for people who want to understand the system or contribute to it.

---

## Design philosophy

MeshCDN was built around five principles. Most architectural decisions trace back to one of these:

### 1. Configuration is commands; commands are backups; backups are sync.

The system has no separate "config file format" and "config sync protocol." There's one thing — a small command language (`/w`, `/d`, `/v`) — and everything is expressed in it. Exporting your cluster's configuration produces a list of commands. Restoring it means replaying them. Syncing two nodes means broadcasting the difference.

This means:
- The "data model" is a sequence of commands, not a schema. Schemas exist (SQLite), but they're a cache of the command history.
- Disaster recovery is trivial: paste the export back in.
- Onboarding new nodes is trivial: ship them the export.

### 2. The command language is the product. Telegram is just one terminal.

The product is the `/w /d /v` command set, not the Telegram interface. Telegram happens to be the default frontend because it's free, ubiquitous, and provides a built-in audit log (group history). But you can run the same commands via:

- `cdn-agent exec "/w domain ..."` — local CLI
- The mesh broadcast layer — peer-to-peer
- Future: a web UI, API clients

The CLI was deliberately built first, **before** the Telegram interface. If we'd built Telegram first, we'd have ended up with command logic tangled into Telegram callbacks.

### 3. Equal peers. No master.

Every node holds the full configuration. Every node can execute commands. There is no leader election, no master node, no "primary" anywhere. The "Bot node" — the one currently polling Telegram for new commands — can drift to any peer if the current one fails, and the cluster keeps working.

This eliminates a whole class of failures (split-brain, leader election bugs, master-slave replication lag) at the cost of doing slightly more work per command (broadcast everywhere instead of replicating from leader). For a CDN with O(10) nodes and O(100) domains, this tradeoff is correct.

### 4. Bot → node is unidirectional. Node ↔ node is bidirectional.

The Bot node is the only one connecting to Telegram. Other nodes do not connect to Telegram at all — they don't even create a Telegram client object. This is critical for nodes in network-restricted regions: they can serve traffic, sync configuration, and hold certificates without depending on external API access.

Between nodes, communication is fully bidirectional over a single port (9443) using HTTP+JSON with bearer-token auth.

### 5. Minimal attack surface.

There is no web admin panel. There is no public API. There is no SSH dependency. The only ports exposed publicly are:

- **80, 443** — CDN traffic (and ACME validation on port 80)
- **9443** — mesh communication (peer-to-peer only; doesn't accept commands from the public internet)
- **Custom ports** — whatever you've configured for your CDN endpoints

Management happens via Telegram, which means your "admin interface" is your phone. You can't accidentally expose the admin panel to the public internet because there is no admin panel.

---

## System overview

```
                        ┌─────────────────────┐
                        │  Telegram group     │
                        │  (control surface)  │
                        └──────────┬──────────┘
                                   │ commands & button taps
                                   │ (long-polling)
                                   ▼
                  ┌─────────────────────────────┐
                  │  Bot node                   │
                  │  (any one of the peers,     │
                  │   typically join_order=1)   │
                  └────────────┬────────────────┘
                               │
                               │ HTTP+JSON :9443
                               │ (bearer-token auth)
              ┌────────────────┼────────────────┐
              │                │                │
              ▼                ▼                ▼
          ┌────────┐      ┌────────┐      ┌────────┐
          │ Node A │ ◄──► │ Node B │ ◄──► │ Node C │
          │        │      │        │      │        │
          │ ┌────┐ │      │ ┌────┐ │      │ ┌────┐ │
          │ │SQL │ │      │ │SQL │ │      │ │SQL │ │
          │ │ite │ │      │ │ite │ │      │ │ite │ │
          │ └────┘ │      │ └────┘ │      │ └────┘ │
          │ ┌────┐ │      │ ┌────┐ │      │ ┌────┐ │
          │ │ngx │ │      │ │ngx │ │      │ │ngx │ │
          │ └────┘ │      │ └────┘ │      │ └────┘ │
          └───┬────┘      └───┬────┘      └───┬────┘
              │               │               │
              ▼               ▼               ▼
          end users       end users       end users
```

Each node runs two processes:

1. **`cdn-agent`** — A single Go binary. Manages config, talks to other peers, optionally connects to Telegram. Generates nginx config files. Reloads nginx when config changes.

2. **OpenResty** (nginx + LuaJIT) — The actual reverse proxy. Serves traffic, runs Lua-based defense rules, terminates SSL. Configured by `cdn-agent`.

Internally, `cdn-agent` is structured as several packages:

```
cmd/cdn-agent/
└── main.go              startup, signal handling, dispatch

internal/
├── config/              load /etc/meshcdn/config.json
├── database/            SQLite schemas and migrations (config.db, logs.db)
├── cdn/                 OpenResty config generation, cert lifecycle
├── acme/                ACME client (Let's Encrypt + ZeroSSL)
├── mesh/                peer-to-peer protocol (server + client)
├── bot/                 Telegram interface, command parsing, AI
├── cli/                 cdn-agent exec entry point (dispatcher)
└── logs/                access log collection and stats aggregation
```

---

## The mesh layer

The mesh is what makes MeshCDN a cluster instead of a collection of independent nodes.

### Peer discovery

A new node joins by presenting a shared secret to any existing peer:

```
secret = sha256(group_id + bot_token)
```

Both sides know `group_id` and `bot_token`. The hash is one-way — it doesn't expose either input on the wire. Once authenticated, the new node receives:

1. The current peer list (so it knows who else is in the cluster)
2. A full configuration export (replayed locally to rebuild state)
3. Its `join_order` number

That's it. The new node is now a full peer.

### Config sync

Two mechanisms:

**Live broadcast** — Whenever any node executes a `/w` or `/d` command, it broadcasts the raw command to every other peer. The command is executed locally on each peer. This is sub-second latency under normal conditions.

**Heartbeat reconciliation** — Every minute, peers exchange three version numbers:

```
cluster_version    — incremented on peer add/remove
routing_version    — incremented on /w domain, /w port, /w ssl
policy_version     — incremented on /w cache, /w defense, /w header
```

If two peers disagree on any version, the higher-version side pushes the relevant config to the lower-version side. This recovers from missed broadcasts (network blip, peer restart, etc.).

The three-stream split exists so that a flurry of `/w cache` commands doesn't invalidate routing, and vice versa.

### Bot drift

The "Bot node" — the one polling Telegram — is whichever peer is currently doing it. There are three ways the role can move:

1. **Automatic**: if the current Bot node misses 3 consecutive heartbeats (≥3 minutes silent), the peer with the smallest `join_order` among the survivors takes over.
2. **Manual**: send `/target <peer-ip>` from any peer to deliberately move the role.
3. **Emergency**: SSH into any peer and run `cdn-agent takeover`.

Bot drift uses dynamic creation of the Telegram client. Non-Bot nodes never instantiate the Telegram client, so they don't try to connect to `api.telegram.org` (and don't fail in environments where that connection isn't possible).

### Wire protocol

For details, see [mesh-protocol.md](mesh-protocol.md). The short version:

- HTTP+JSON over port 9443
- Bearer token auth using the same `sha256(group_id + bot_token)` derivation
- Endpoints: `/mesh/auth`, `/mesh/exec`, `/mesh/ping`, `/mesh/export`, `/mesh/peers`, `/mesh/takeover`, `/mesh/bootstrap` (for new-node provisioning), `/mesh/download` (for binary distribution during cluster-wide upgrades)

---

## Why Telegram as the control plane?

This is the most-questioned design decision, so it deserves explanation.

**Pros of Telegram:**

- **Free**: zero infrastructure cost.
- **Mobile-first**: operations are doable from a phone in a coffee shop. This matters for small teams without a NOC.
- **Audit log built in**: group history is the immutable record of every command anyone ran. Combined with member permissions, this gives accountability without building it.
- **Notification channel built in**: alerts (cert expiry, reload failures, peer drops) go to the same place commands come from.
- **No public-facing admin port**: Telegram polls outbound. Even if your admin password leaked, attackers couldn't reach an admin endpoint because there isn't one. They'd need to compromise your Telegram account.
- **Multi-user, multi-device**: every operator on the team gets a working interface for free.

**Cons:**

- **Dependency on a third party**: if Telegram is unreachable, you can't control the cluster via Telegram. (Mitigation: `cdn-agent exec` CLI works locally on any node, regardless of Telegram availability.)
- **Privacy mode trap**: Telegram bots default to privacy-mode-on, which means they only see commands starting with `/`. If you want bot-mention-style AI queries, you must disable privacy mode or make the bot a group admin. (Documented in [deployment.md](deployment.md).)
- **Not great for very long output**: Telegram messages are capped at 4096 characters. For long output (e.g. `/v export` of a large cluster), we paginate.

We considered the alternatives:

- **Web admin panel** (rejected): the moment you have one, you have an admin port to defend. Authentication, sessions, CSRF, brute force protection — all things to get wrong. And it doesn't work from a phone in a coffee shop without a VPN.
- **SSH-based control** (rejected): operators need SSH access to all nodes. Key management at multi-node scale is a chore. And again, no notification channel.
- **REST API + custom CLI** (deferred): we built the local CLI (`cdn-agent exec`) so the option exists, but a remote API would just be re-implementing Telegram with more code. Maybe in v3.2 if there's demand.

For now, Telegram is the default. The architecture supports replacing it (the command language is the product, not Telegram), but no other frontend is implemented yet.

---

## SSL certificate management

Certificates are split into two categories with different policies:

### Per-node IP certificates

Each node has a self-issued certificate for its public IP. These are used for mesh communication and as the fallback certificate for any port without a domain match.

- Validity: 6 days (short, because they're cheap to renew and we want fast key rotation if anything goes wrong)
- Renewal: every 6 hours, renew if remaining < 3 days
- Storage: `/etc/meshcdn/certs/<ip>.crt|.key`, **not** synced across the cluster (each node has its own)
- Fallback: if Let's Encrypt fails (LE doesn't sign IPs in the public CA), drop to self-signed. This is normal and expected.

### Per-domain Let's Encrypt certificates

Domain certificates are managed centrally by the Bot node and synced cluster-wide.

- Validity: 90 days (Let's Encrypt standard)
- Renewal: every 6 hours, renew if remaining < 7 days
- Storage: `/etc/meshcdn/certs/<domain>.crt|.key` on every node
- Sync: when the Bot node renews, it broadcasts the new certificate (PEM bundles, base64-encoded) to all peers. Each peer writes the new files and reloads nginx.

### State machine (v3.1)

A certificate has a `live_status` indicating whether the disk version matches what nginx is actually serving:

| State | Meaning |
|---|---|
| `live` | Disk fingerprint matches nginx-loaded fingerprint. Healthy. |
| `pending_reload` | Just renewed, reload not yet completed. Transient. |
| `renewed_not_live` | Disk has new cert, but nginx reload failed. **This is the alert state**: production is still serving the old cert, which will eventually expire. |
| `stale` | Old, hasn't renewed when expected. |
| `unknown` | Pre-v3.1 records. Auto-promotes to `live` after first successful renewal. |

The `renewed_not_live` state is the most operationally important: it surfaces the silent failure where your renewal succeeded on disk but nginx couldn't reload (e.g. because some other config error is blocking it). Without this state, you'd discover the problem when the old cert expires and your site goes down.

A Telegram alert fires when this state appears. `/v ssl health` shows it visually.

### manifest.json — the source of truth

`/etc/meshcdn/certs/manifest.json` is a metadata index of all certificates (current + history). It's the source of truth for:

- Which CA issued each cert
- Cryptographic fingerprints (for state-machine comparison)
- Expiry dates
- Historical certs (last 5 versions per domain, in case you need to roll back)

The database has the same data, but `manifest.json` survives `install.sh --upgrade` (which currently clears the DB). On startup, if `manifest.json` exists but the DB doesn't have the records, they're rebuilt from manifest. If manifest is missing but cert files exist on disk, manifest is rebuilt by scanning the directory.

This redundancy is deliberate: certificates are the most important state in the system (losing them means downtime), so we have two independent ways to recover.

---

## AI integration

MeshCDN ships with an optional AI assistant that can answer natural-language questions about the cluster.

### Core principles

1. **Read-only by design**. The AI can issue SQL queries against the read replica of the config DB. It cannot execute `/w` or `/d` commands. Ever.
2. **Suggestions, not actions**. If you ask "should I increase the cache TTL on a.com?", the AI will give a recommendation. It will not actually change the TTL.
3. **Operator stays in control**. Every change to the cluster goes through a human running an explicit command.

This is intentional. The risk of an AI making a configuration change in production — even with the best intentions — is much higher than the value it adds. Read-only assistance is the sweet spot.

### How it works

When a user `@mentions` the bot in the Telegram group:

1. The mention is detected, the question is extracted
2. The question + a system prompt + the schema of the cluster's databases are sent to the configured LLM provider (DeepSeek and OpenAI are supported; pluggable)
3. The LLM may respond with `<sql>SELECT ...</sql>` blocks
4. Those queries are executed against the local read-only DB
5. Results are sent back to the LLM as additional context
6. The LLM produces a final answer in natural language
7. The answer is posted back to Telegram

For cross-node queries, the syntax `@<peer-ip> <question>` routes the SQL execution to that peer. This is useful for "are the configs really the same on every node?" type questions.

### Patrol mode

Every 6 hours, the AI runs a self-directed health check against the cluster: looking for certs near expiry, configurations that look suspicious, traffic anomalies in the access logs. It posts a summary to the group. This is again read-only and advisory.

---

## Configuration layering

Configuration on a MeshCDN node is split into two layers:

### Permanent layer (preserved across upgrades)

```
/etc/meshcdn/config.json     # node identity, bot token, ai keys, peer addr
/etc/meshcdn/certs/          # SSL certificates and manifest.json
/etc/meshcdn/peers.json      # peer whitelist (for auth)
/etc/meshcdn/backups/        # automatic pre-upgrade backups
```

These files survive `install.sh --upgrade`. They contain irreplaceable state (your bot token, your private keys, your cert history).

### Rebuildable layer (cleared on upgrade)

```
/etc/meshcdn/config.db       # main configuration database
/etc/meshcdn/logs.db         # access logs and AI conversation history
/etc/meshcdn/nginx/          # generated nginx config
/etc/meshcdn/cache/          # nginx cache directory
/etc/meshcdn/logs/           # access logs (raw)
/etc/meshcdn/welcome/        # default-server welcome page
/etc/meshcdn/challenges/     # ACME validation files
```

These are reconstructed on first run after upgrade by:
1. Running schema migrations
2. Pulling full config from another peer (mesh sync)
3. Regenerating nginx config from the now-populated DB
4. Reloading nginx

This split exists so that schema changes can be implemented as "drop and rebuild from peers" rather than "ALTER TABLE" — much simpler at the cost of requiring at least one healthy peer to recover. Single-node deployments rely on the automatic pre-upgrade backups instead.

(Future versions will probably move toward "ALTER TABLE only, never drop" for the rebuildable layer too.)

---

## Comparison with alternative approaches

### vs. nginx + Certbot

You can absolutely build something CDN-like with raw nginx + Certbot + Ansible. Many sites do. The pain points MeshCDN addresses:

- **Multi-node sync**: with nginx + Certbot, you need to invent your own config sync (rsync, Ansible, custom scripts). MeshCDN does it natively.
- **Cert sync**: same — you need to coordinate cert renewal across nodes. MeshCDN renews on the Bot node and broadcasts.
- **Operations interface**: `vim /etc/nginx/sites-available/example.com` works, but it doesn't scale to a cluster. MeshCDN's `/w` commands sync everywhere.
- **State observability**: with raw nginx, "is my cert really live everywhere?" is a manual check on each box. MeshCDN tracks it.

### vs. Caddy

Caddy is wonderful — single binary, automatic SSL, simple config. The differences:

- **Multi-node**: Caddy doesn't have native multi-node sync. You can use Caddy + a config sync layer (like Caddyfile-on-S3), but it's bolt-on.
- **Mesh networking**: Caddy doesn't have peer-to-peer. MeshCDN nodes talk to each other.
- **Control plane**: Caddy expects you to manage it via files and API. MeshCDN expects you to manage it via chat.
- **Use cases**: if you have a single server, Caddy is simpler and probably what you want. If you have ≥2 servers and want them to act as a single CDN, MeshCDN starts to win.

### vs. Cloudflare / Akamai / Fastly

You're not directly comparing — these are global commercial CDNs with massive networks. MeshCDN can't replicate that infrastructure. But for use cases where:

- You don't need 300 PoPs, just 3-10 strategic locations
- You need control over the proxy software (custom Lua rules, specific TLS configurations, etc.)
- Cost matters (a cluster of $5/mo VPS instances vs. enterprise CDN contracts)
- Provider independence matters (compliance, sovereignty, "we don't want our traffic going through that provider")

…MeshCDN gives you a self-controlled alternative.

---

## Protocol convergence: an observation

A subtle property of MeshCDN's design is worth noting because it has implications for use cases beyond traditional CDN.

A CDN, fundamentally, is a system that:
1. Accepts client connections on standard HTTPS
2. Forwards traffic to upstream services
3. Optionally caches/transforms responses
4. May route through multiple intermediate nodes

A coordination/tunneling system has the same shape:
1. Accepts client connections (often on standard HTTPS, to blend in)
2. Forwards to upstream services
3. May route through multiple nodes

These two functions are technically equivalent at the protocol level. Once you have a multi-node mesh that proxies HTTPS through configurable origin paths (which any real CDN must support — it's the basis of failover and origin shielding), you have all the primitives needed for both.

MeshCDN focuses on CDN — that's the design intent — but the architecture doesn't artificially restrict it. Communication between nodes uses ordinary HTTPS on a configurable port, and origin path configuration supports arbitrary peer chains. Operators interested in additional uses can configure them by composing existing primitives; we don't need to add separate protocol logic for that.

This convergence is a positive design property: a single set of primitives serves multiple needs, with a single attack surface to audit.

---

## Things we got wrong (and learned from)

A few decisions we'd reconsider:

- **`@name` for rule template references** — initially used `@`, but Telegram's user-mention parser interfered with messages containing `@xxx` tokens that happened to look like usernames. Changed to `#name` (hashtag-style) in v3.1.0-alpha6. Lesson: when designing a chat-native command syntax, test against the chat platform's parsers, not just your own.

- **`install.sh --upgrade` clearing the database** — this works in multi-node clusters (peers heal it via mesh sync) but bites hard in single-node setups. v3.2 will keep the database across upgrades by default, with an opt-in clean-install flag.

- **Initial v2 Telegram-only bias** — early versions made command logic call Telegram reply functions inline, making it hard to use the same logic from CLI. The v3.0 refactor extracted command logic into pure functions called by Telegram or CLI as a thin layer.

These are documented in the changelog and in code comments. Future contributors should feel free to challenge any current design choice if a better one is available.

---

## Where to read next

- [mesh-protocol.md](mesh-protocol.md) — exact wire protocol for peer-to-peer
- [configuration.md](configuration.md) — full command reference
- [deployment.md](deployment.md) — how to actually run it
- [faq.md](faq.md) — common questions
