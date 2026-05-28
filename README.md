# MeshCDN

> **A self-hosted, peer-to-peer CDN with no central control plane.**

MeshCDN turns a handful of small VPS nodes (across any cloud provider, in any
country) into a working CDN cluster — without depending on a single commercial
provider, without a central control plane, and without exposing any management
interface to the public internet.

> **Version note**
> This is **MeshCDN v4**, a complete ground-up rewrite in Go. Earlier v2.x/v3.x
> lines were a different codebase; their documentation is preserved under the
> release [`v3.1.0`](../../releases/tag/v3.1.0) for historical reference. v4
> shares the *concepts* (equal-peer mesh, Telegram control plane, snapshot-replay
> config) but none of the v3 code.

---

## Why this exists

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

## Core features

- **Equal-peer architecture** — Every node holds the full configuration and can
  execute commands. No master, no leader election, no split-brain. The "Bot node"
  (the one currently polling Telegram) can be moved to any peer.
- **Telegram Bot as control plane** — All operations through a Telegram group
  chat. Group history doubles as an audit log. No web panel, no SSH dependency,
  zero exposed admin surface.
- **Commands-as-configuration** — There is no separate "config format". The
  command language *is* the configuration. Exporting produces a list of commands;
  restoring replays them; syncing broadcasts the difference.
- **Live cluster sync** — Changes broadcast in real time; missed updates
  reconciled by a 1-minute heartbeat with a monotonic version counter.
- **Distributed SSL management** — Per-node IP certs and per-domain Let's
  Encrypt certs (via [lego](https://github.com/go-acme/lego)). Upload your own
  certs (auto-detected by PEM content, multi-SAN supported, cert/key pair
  validated). Self-signed fallback.
- **Multi-provider AI assistant** (optional) — Ask in natural language. Supports
  OpenAI, Gemini, Claude, DeepSeek, Grok, and Qwen. Read-only and advisory: it
  suggests commands, you decide whether to run them. It never executes anything
  itself. (You can ask in any language — the assistant replies in the language
  you use, regardless of the documentation language.)
- **File-based config I/O** — Export the whole cluster config as a `.txt`
  attachment; upload a `.txt` back to re-apply it (with a confirmation preview).
- **Application-layer rules** — cache rules, header rewriting, redirects, and
  IP / referer / rate / size-based defense.
- **Atomic upgrades with rollback** — `runtime/` is rebuilt from `persistent/`
  on every upgrade; the four persistent files are never destroyed.
- **Single binary** — Go-compiled, builds to one `cdn-agent` binary.

---

## Quick start (single node)

There are no prebuilt release binaries yet — you build from source on the node.
The repository already contains the full Go source, so the flow is: build the
binary, then run the installer (which finds the binary you just built).

```bash
# 1. Clone the repository onto the node
git clone https://github.com/shellylittleant/MeshCDN.git
cd MeshCDN

# 2. Build the binary (requires Go 1.21+; install Go first if missing)
make build                 # produces ./cdn-agent

# 3. Install as the first node
cp cdn-agent scripts/      # the installer looks for the binary alongside itself
sudo bash scripts/install.sh \
  --bot-token="<your_telegram_bot_token>" \
  --group-id="<your_telegram_group_id>"
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

### Adding a second node

On the new machine, build the binary the same way, then run the bootstrap
script pointed at any existing peer:

```bash
git clone https://github.com/shellylittleant/MeshCDN.git
cd MeshCDN
make build
cp cdn-agent scripts/
sudo bash scripts/bootstrap.sh \
  --bot-token="<token>" \
  --group-id="<group_id>" \
  --peer="<existing-node-ip>"
```

The new node authenticates via a shared secret derived from
`sha256(group_id + bot_token)`, pulls the full config, and joins the mesh.

---

## Architecture overview

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

### Three subsystems

V4 organizes everything by lifecycle:

- **Skeleton** — what sits on disk. A `persistent/` directory (identity, peers,
  certs, config snapshot — survives upgrades) and a `runtime/` directory (SQLite
  DB, generated nginx config, cache — rebuilt from `persistent/` on each upgrade).
- **Muscle** — the command system. A strict four-segment grammar
  (`/<verb> <type> <scope> <params>`), batch transactions, one handler per type.
- **Blood** — the mesh. HTTP+JSON over port 9443 with bearer-token auth.

See [docs/V4-DESIGN.md](docs/V4-DESIGN.md) for the full design rationale.

---

## The command model

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
/w cache img-7d patterns=*.jpg,*.png ttl=604800          # define a cache object
/w bind example.com cache:img-7d                         # bind the object to a domain
/v export - -                                            # export full config (as a file)
/v status - -                                            # node status
```

Run the same commands from any node's CLI:

```bash
sudo cdn-agent exec "/w domain https://example.com 443 https://1.2.3.4:443"
```

For the full command reference, see [docs/V4-DESIGN.md](docs/V4-DESIGN.md) §8.

---

## Status

**Alpha — in active production testing.** Run by the author across real VPS
nodes in multiple regions. Usable for non-critical workloads today.

What's working:

- Equal-peer mesh, peer auth, live broadcast + heartbeat sync
- SSL lifecycle: Let's Encrypt (IP + domain), user-uploaded certs (PEM
  auto-detection, multi-SAN, cert/key pair validation), self-signed fallback
- Command surface: `domain`, `ssl`, `sslfile`, `cache`, `header`, `redirect`,
  `defense`, `bind`, plus management (`export`, `sync`, `target`, `upgrade`,
  `nodes`, `status`)
- Multi-provider AI assistant (OpenAI / Gemini / Claude / DeepSeek / Grok / Qwen),
  read-only/advisory
- File-based config export/import via Telegram
- Cluster-wide upgrades; `runtime/` rebuilt from `persistent/`

Not yet rebuilt from v3.x (planned):

- Rule templates (`#name` references) — superseded by the object/bind model
- Smart origin routing (ordered failover paths through peers)
- Bulk origin replacement, node-level redirects

---

## Documentation

| Document | Topic |
|---|---|
| [docs/V4-DESIGN.md](docs/V4-DESIGN.md) | The V4 "constitution": architecture, command grammar, schema, design rationale |
| [docs/AI-PRIMER.md](docs/AI-PRIMER.md) | Condensed overview of the system — start here if you're an AI tool or a new contributor |

---

## Building from source

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

## License

[Apache License 2.0](LICENSE).

Use MeshCDN for any purpose, commercial or otherwise. If you fork it, please
keep the copyright notices intact.

---

## Acknowledgments

- [OpenResty](https://openresty.org/) — nginx + LuaJIT, the proxy engine
- [Let's Encrypt](https://letsencrypt.org/) — free TLS certificates
- [go-acme/lego](https://github.com/go-acme/lego) — ACME client library
- [go-telegram/bot](https://github.com/go-telegram/bot) — Telegram client library
- [SQLite](https://sqlite.org/) — embedded database
