# MeshCDN

> **A self-hosted, peer-to-peer CDN with no central control plane.**
> 一个完全自建、节点对等的分布式 CDN 系统。

MeshCDN turns a handful of small VPS nodes (across any cloud provider, in any
country) into a working CDN cluster — without depending on a single commercial
provider, without a central control plane, and without exposing any management
interface to the public internet.

MeshCDN 把分散在各家云厂商、不同地区的小型 VPS 节点组成一个可用的 CDN
集群——不依赖任何商业 CDN，没有中心化控制面，所有管理入口都不暴露在公网上。

> **Note / 版本说明**
> This is **MeshCDN v4**, a complete ground-up rewrite in Go. Earlier v2.x/v3.x
> lines were a different codebase; their documentation is preserved under the
> git tag [`archive/v3.1-docs`](../../tree/archive/v3.1-docs) for historical
> reference. v4 shares the *concepts* (equal-peer mesh, Telegram control plane,
> snapshot-replay config) but none of the v3 code.
>
> 这是 **MeshCDN v4**——用 Go 从零重写的版本。v2.x/v3.x 是另一套代码库，其文档
> 保留在 git tag `archive/v3.1-docs` 下供参考。v4 沿用了设计**理念**，但代码完全重写。

---

## Why this exists / 为什么要做这个

Today's web depends heavily on a small number of commercial CDN providers. For
most sites that's fine. But for operators who need:

- **Geographic redundancy** without locking in to one provider's pricing and policies,
- **Self-hosted operation** for compliance, sovereignty, or simply preference,
- **Infrastructure-level flexibility** — e.g. routing traffic through specific regions,
- A **low-cost path** to running on cheap VPS instances rather than enterprise contracts,

…the existing options are surprisingly thin. You're either paying for
Cloudflare/Akamai/Fastly, or hand-rolling nginx + Let's Encrypt + ad-hoc
deployment scripts on each box.

MeshCDN is a third option: **a CDN you operate yourself, with the operational
ergonomics of a commercial product** (single-command deployment, automatic SSL,
cluster-wide config sync), but where every node is yours.

---

## Core features / 核心特性

- 🌐 **Equal-peer architecture** — Every node holds the full configuration and can
  execute commands. No master, no leader election, no split-brain. The "Bot node"
  (the one currently polling Telegram) can drift to any peer on failure.
- 📱 **Telegram Bot as control plane** — All operations through a Telegram group
  chat. Group history doubles as an audit log. No web panel, no SSH dependency,
  zero exposed admin surface.
- 🧱 **Commands-as-configuration** — There is no separate "config format". The
  command language *is* the configuration. Exporting produces a list of commands;
  restoring replays them; syncing broadcasts the difference.
- 🔄 **Live cluster sync** — Changes broadcast in real time; missed updates
  reconciled by a 1-minute heartbeat with version vectors.
- 🔒 **Distributed SSL management** — Per-node IP certs and per-domain Let's
  Encrypt certs (via [lego](https://github.com/go-acme/lego)). Upload your own
  certs (auto-detected by PEM content, multi-SAN supported). Self-signed fallback.
- 🤖 **Multi-provider AI assistant** (optional) — Ask in natural language. Supports
  OpenAI, Gemini, Claude, DeepSeek, Grok, and Qwen. Read-only and advisory: it
  suggests commands, you decide whether to run them. It never executes anything itself.
- 📤 **File-based config I/O** — Export the whole cluster config as a `.txt`
  attachment; upload a `.txt` back to re-apply it (with a confirmation preview).
- 🛡️ **Application-layer rules** — cache rules, header rewriting, redirects, and
  IP / referer / rate / size-based defense.
- 💾 **Atomic upgrades with rollback** — `runtime/` is rebuilt from `persistent/`
  on every upgrade; the four persistent files are never destroyed.
- 🪶 **Single static binary** — Go-compiled, builds to one `cdn-agent` binary.

---

## Quick start (single node) / 快速开始（单节点）

There are no prebuilt release binaries yet — you build from source on the node
itself. A helper script handles installing Go, fetching dependencies, building,
and installing.

```bash
# 1. Get the source onto the node (clone, or scp a tarball)
git clone https://github.com/shellylittleant/MeshCDN.git
cd MeshCDN

# 2. Build + install as the first node
sudo bash scripts/build-and-install.sh \
  --bot-token="<your_telegram_bot_token>" \
  --group-id="<your_telegram_group_id>" \
  --source=.
```

Then, in your Telegram group:

```
/menu                                                  # main menu
/w domain https://example.com 443 https://1.2.3.4:443  # register a domain
/w ssl example.com -                                   # auto Let's Encrypt
/v domain example.com -                                # inspect it
```

That's it — `example.com` → `1.2.3.4:443` is now served with auto-renewing SSL.

> **Telegram setup**: create a bot via [@BotFather](https://t.me/BotFather),
> disable its privacy mode (or make it a group admin) so it can read commands,
> create a group, add the bot, and get the group ID. See
> [docs/V4-DESIGN.md](docs/V4-DESIGN.md) for details.

### Adding a second node / 加第二个节点

On the new machine, point it at any existing peer:

```bash
sudo bash scripts/build-and-install.sh \
  --bot-token="<token>" \
  --group-id="<group_id>" \
  --peer="<existing-node-ip>" \
  --source=.
```

The new node authenticates via a shared secret derived from
`sha256(group_id + bot_token)`, pulls the full config, and joins the mesh.

---

## Architecture overview / 架构概览

```
                 ┌─────────────────┐
                 │ Telegram group  │   ← control surface (one terminal)
                 └────────┬────────┘
                          │ commands & button taps
                          ▼
           ┌──────────────────────────┐
           │  Bot node                │   ← any one of the peers
           └──────────┬───────────────┘
                      │ HTTP+JSON :9443 (bearer-token auth)
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

Every node runs two processes:

- **OpenResty** (nginx + LuaJIT) — the actual reverse proxy on 80 / 443 / custom ports.
- **`cdn-agent`** (single Go binary) — generates nginx config, manages certs,
  handles mesh sync, and (on the Bot node only) connects to Telegram.

Nodes other than the Bot node never connect to Telegram, so nodes in
network-restricted regions can fully participate — serve traffic, sync config,
hold certificates — without external API access.

### Three subsystems / 三套系统

V4 organizes everything by lifecycle:

- **Skeleton (骨架)** — what sits on disk. A `persistent/` directory (identity,
  peers, certs, config snapshot — survives upgrades) and a `runtime/` directory
  (SQLite DB, generated nginx config, cache — rebuilt from `persistent/` on each upgrade).
- **Muscle (肌肉)** — the command system. A strict four-segment grammar
  (`/<verb> <type> <scope> <params>`), batch transactions, and one handler per type.
- **Blood (血液)** — the mesh. HTTP+JSON over port 9443 with bearer-token auth.

See [docs/V4-DESIGN.md](docs/V4-DESIGN.md) for the full design rationale.

---

## The command model / 命令模型

Everything is a four-segment command, usable identically from Telegram, the CLI,
or peer-to-peer broadcast:

```
/<verb> <type> <scope> <params>
```

- **verbs**: `/w` write (create/update), `/d` delete, `/v` view (read/export)
- **`-`** is the placeholder for an empty segment ("this node" / "no value")

A few examples:

```
/w domain https://example.com 443 https://1.2.3.4:443   # add domain → origin
/w ssl example.com -                                     # issue Let's Encrypt cert
/w sslfile example.com -                                 # upload your own cert (+ attach files)
/w cache example.com *.jpg,*.png 604800                  # cache static assets 7 days
/v export - -                                            # export full config (as a file)
/v status - -                                            # node status
```

Run the same commands from any node's CLI:

```bash
sudo cdn-agent exec "/w domain https://example.com 443 https://1.2.3.4:443"
```

For the full command reference, see [docs/V4-DESIGN.md](docs/V4-DESIGN.md) §8.

---

## Status / 项目状态

**Alpha — in active production testing.** Run by the author across real VPS
nodes in multiple regions. Usable for non-critical workloads today.

What's working:

- Equal-peer mesh, peer auth, live broadcast + heartbeat sync ✓
- SSL lifecycle: Let's Encrypt (IP + domain), user-uploaded certs (PEM
  auto-detection, multi-SAN, cert/key pair validation), self-signed fallback ✓
- Command surface: `domain`, `ssl`, `sslfile`, `cache`, `header`, `redirect`,
  `defense`, plus management (`export`, `sync`, `target`, `upgrade`, `nodes`,
  `status`, `bind`) ✓
- Multi-provider AI assistant (OpenAI / Gemini / Claude / DeepSeek / Grok / Qwen),
  read-only/advisory ✓
- File-based config export/import via Telegram ✓
- Cluster-wide upgrades; `runtime/` rebuilt from `persistent/` ✓

Not yet rebuilt from v3.x (planned):

- Rule templates (`#name` references)
- Smart origin routing (ordered failover paths through peers)
- Bulk origin replacement, node-level redirects

---

## Documentation / 文档

| Document | Topic |
|---|---|
| [docs/V4-DESIGN.md](docs/V4-DESIGN.md) | The V4 "constitution": architecture, command grammar, schema, design rationale |
| [docs/AI-PRIMER.md](docs/AI-PRIMER.md) | How the AI assistant works and how to configure providers |

---

## Building from source / 从源码构建

```bash
# Requires Go 1.21+
make build          # produces ./cdn-agent
make vet            # go vet
make fmt            # gofmt -w
```

The build embeds a snapshot of its own source (for self-containment); this is
generated at build time and is not committed to the repository.

> The Go module path in `go.mod` is currently `github.com/example/meshcdn`. If
> you intend to `go install` or import packages, change it to match this repo.

---

## License / 许可

[Apache License 2.0](LICENSE).

Use MeshCDN for any purpose, commercial or otherwise. If you fork it, please
keep the copyright notices intact.

---

## Acknowledgments / 致谢

- [OpenResty](https://openresty.org/) — nginx + LuaJIT, the proxy engine
- [Let's Encrypt](https://letsencrypt.org/) — free TLS certificates
- [go-acme/lego](https://github.com/go-acme/lego) — ACME client library
- [go-telegram/bot](https://github.com/go-telegram/bot) — Telegram client library
- [SQLite](https://sqlite.org/) — embedded database