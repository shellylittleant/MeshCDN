# MeshCDN

> **A self-hosted, peer-to-peer CDN with no central control plane.**
> 一个完全自建、节点对等的分布式 CDN 系统。

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Report](https://img.shields.io/badge/go-1.21%2B-blue.svg)]()
[![Status](https://img.shields.io/badge/status-alpha-orange.svg)]()

MeshCDN turns a handful of small VPS nodes (across any cloud provider, in any country) into a working CDN cluster — without depending on a single commercial provider, without a central control plane, and without exposing any management interface to the public internet.

MeshCDN 把分散在各家云厂商、不同地区的小型 VPS 节点组成一个可用的 CDN 集群——不依赖任何商业 CDN，没有中心化控制面，所有管理入口都不暴露在公网上。

---

## Why this exists / 为什么要做这个

Today's web depends heavily on a small number of commercial CDN providers. For most sites that's fine. But for organizations that need:

- **Geographic redundancy** without locking in to one provider's pricing and policies,
- **Self-hosted operation** for compliance, sovereignty, or simply preference,
- **Infrastructure-level flexibility** — e.g. routing traffic through specific regions for latency reasons,
- A **low-cost path** to running on cheap VPS instances rather than enterprise contracts,

…the existing options are surprisingly thin. You're either paying for Cloudflare/Akamai/Fastly, or you're hand-rolling nginx + Let's Encrypt + ad-hoc deployment scripts on each box.

MeshCDN is a third option: **a real CDN you operate yourself, with the same operational ergonomics as a commercial product** (single-command deployment, automatic SSL, cluster-wide config sync), but where every node is yours.

---

## Core features / 核心特性

- 🌐 **Equal-peer architecture** — Every node holds the full configuration and can execute commands. No master, no leader election, no split-brain.
- 📱 **Telegram Bot as control plane** — All operations through a Telegram group chat. Group history doubles as audit log. No web panel, no SSH dependency, zero exposed admin surface.
- 🔄 **Live cluster sync** — Configuration changes broadcast in real time; missed updates reconciled by 1-minute heartbeat with three-stream version vectors.
- 🔒 **Distributed SSL management** — Per-node IP certs (auto-renewed every 6 days), per-domain Let's Encrypt certs synced across the cluster. Built-in fallbacks: ZeroSSL, self-signed.
- 🛡️ **Application-layer defense** — IP allow/deny, rate limiting, referrer filtering, request size limits, CC protection (Lua-based sliding window).
- 📋 **Rule templates** — Define cache/redirect/header/defense rule sets once with `#name`, reference from any domain.
- 🔀 **Smart origin routing** — Configure ordered failover paths through specific peer nodes per origin (`direct → relay-via-NodeIP → ...`).
- 🤖 **AI assistant** (optional) — Ask in natural language. The assistant reads cluster config to answer questions and offer suggestions, but never executes commands itself.
- 💾 **Atomic upgrades with rollback** — Every upgrade automatically backs up the database; one-command restore if something goes wrong.
- 🪶 **Single static binary** — Go-compiled, ~5 MB, runs on any Linux x86_64. No runtime dependencies.

---

## Quick start (single node) / 快速开始（单节点）

A single node is fully functional. You can add more nodes later — they'll auto-join the cluster.

```bash
# 1. Download release
wget https://github.com/<org>/meshcdn/releases/latest/download/meshcdn-linux-amd64.tar.gz
tar xzf meshcdn-linux-amd64.tar.gz

# 2. Install
sudo bash install.sh \
  --bot-token="<your_telegram_bot_token>" \
  --group-id="<your_telegram_group_id>"
```

Then in your Telegram group:

```
/menu                                       # see the main menu
/w domain https://example.com 443 https://1.2.3.4:443
/w ssl example.com                          # auto Let's Encrypt
/v domain example.com                       # see everything about it
```

That's it. example.com → 443 → 1.2.3.4:443 is now serving with auto-renewing SSL.

### Adding a second node / 加第二个节点

On the new machine:

```bash
curl -s http://<existing-node-ip>:9443/mesh/bootstrap | \
  sudo bash -s -- <existing-node-ip> "<token>" <group_id>
```

The new node automatically: installs OpenResty if needed → fetches the binary from the existing node → authenticates via shared secret → pulls full config → joins the mesh. Done.

See [docs/deployment.md](docs/deployment.md) for production setup, multi-region considerations, and DNS integration.

---

## Architecture overview / 架构概览

```
                        ┌─────────────────┐
                        │ Telegram group  │
                        │  (control UI)   │
                        └────────┬────────┘
                                 │ commands & button taps
                                 ▼
                  ┌──────────────────────────┐
                  │  Bot node                │
                  │  (any one of the peers)  │
                  └──────────┬───────────────┘
                             │ HTTP+JSON :9443
            ┌────────────────┼────────────────┐
            │                │                │
            ▼                ▼                ▼
        ┌───────┐        ┌───────┐        ┌───────┐
        │ Node A│ ◄────► │ Node B│ ◄────► │ Node C│
        └───────┘        └───────┘        └───────┘
            │                │                │
            ▼                ▼                ▼
       end users        end users        end users
```

Every node runs:

- **OpenResty** (nginx + LuaJIT) — the actual reverse proxy serving traffic on 80/443/custom ports.
- **cdn-agent** (single Go binary) — manages OpenResty config, handles mesh sync, optionally connects to Telegram.

Nodes outside the chosen "Bot node" never connect to the Telegram API. This means nodes in network-restricted regions can participate fully in the cluster (serve traffic, sync config, hold certificates) without being blocked from external services.

For details: [docs/architecture.md](docs/architecture.md)

---

## Status / 项目状态

**Alpha — feature-complete, in active production testing.**

The project has been in iterative development for over a year, with a stable v2.x line running real production traffic and v3.x adding rule templates, smart routing, certificate hardening, and a refactored UI.

Current version: **v3.1.0-alpha11**.

What's solid:
- Mesh networking, peer authentication, config sync ✓
- SSL certificate lifecycle (Let's Encrypt + ZeroSSL + self-sign fallback) ✓
- Five-category command surface (`/w`, `/d`, `/v`, plus management commands) ✓
- Atomic upgrades with automatic backup + one-command rollback ✓
- AI assistant integration (read-only, advisory) ✓

What's still hardening:
- Single-node-cluster upgrade flow (works, but config restore relies on backup rather than mesh peer recovery)
- Production load benchmarks
- More extensive documentation for non-Chinese contributors

What's planned: see [Roadmap](#roadmap).

---

## Documentation / 文档

| Document | Topic |
|---|---|
| [docs/architecture.md](docs/architecture.md) | System design, mesh topology, design tradeoffs |
| [docs/deployment.md](docs/deployment.md) | Installing, joining nodes, DNS setup, troubleshooting |
| [docs/configuration.md](docs/configuration.md) | Command reference, config file structure |
| [docs/mesh-protocol.md](docs/mesh-protocol.md) | Wire-level details of the peer protocol |
| [docs/faq.md](docs/faq.md) | Frequently asked questions |

---

## Roadmap

### Near term (next 3 months)

- v3.1.0 stable release (currently alpha11; mostly stability fixes left)
- Generic DNS provider integration (Cloudflare, Aliyun, DNSPod, AWS Route53) for automatic node-failure DNS removal
- Production load benchmarks
- English-first documentation

### Mid term (3–9 months)

- Smart routing v2: live latency probing between nodes and automatic routing decisions
- mTLS for mesh communication (currently bearer token over HTTPS)
- Optional WebUI as an alternative to the Telegram interface
- Web-based config editor (read-only first, then write)

### Long term (12+ months)

- Pluggable observability backends (Prometheus, OpenTelemetry)
- Multi-tenant operation (multiple isolated clusters on shared hardware)
- Pluggable storage backends for config (currently SQLite-only)

---

## How is this different from existing tools?

| | MeshCDN | Cloudflare / Akamai | nginx + Certbot manual | Caddy |
|---|---|---|---|---|
| Self-hosted | ✓ | ✗ | ✓ | ✓ |
| Multi-node automatic | ✓ | N/A | ✗ (manual sync) | partial (with extras) |
| No web admin exposed | ✓ | ✗ | ✗ | ✗ |
| Decentralized — no master | ✓ | ✗ | ✗ | ✗ |
| Auto cert renewal | ✓ | ✓ | partial | ✓ |
| Config-as-commands export | ✓ | ✗ | ✗ | partial |
| Single binary deploy | ✓ | N/A | ✗ | ✓ |
| Cross-cloud-provider mesh | ✓ | N/A | ✗ | ✗ |

The closest comparison isn't really another CDN — it's "what would you build if you wanted Cloudflare's operational simplicity but on hardware you control yourself, across several VPS providers?"

---

## License

[Apache License 2.0](LICENSE).

You can use MeshCDN for any purpose, commercial or otherwise. If you fork it, please keep the copyright notices intact and let us know — we'd love to hear what you're building.

## Acknowledgments

MeshCDN builds on top of fantastic open-source work:

- [OpenResty](https://openresty.org/) — nginx + LuaJIT, the actual proxy engine
- [Let's Encrypt](https://letsencrypt.org/) — free TLS certificates
- [go-telegram/bot](https://github.com/go-telegram/bot) — Telegram client library
- [SQLite](https://sqlite.org/) — embedded database

And to everyone running early-alpha nodes and reporting bugs: thank you.

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Bug reports especially welcome — this is alpha software.
