# MeshCDN

> **A self-hosted, peer-to-peer CDN with no central control plane.**

MeshCDN turns a handful of small VPS nodes (across any cloud provider, in any
country) into a working CDN cluster — without depending on a single commercial
provider, without a central control plane, and without exposing any management
interface to the public internet.

> **Version note**
> Current release: **v4.4.0** — the interface now speaks English. Send `/en`
> or `/cn` in the group to switch; the setting syncs across the cluster.
>
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

One command, three inputs: your bot token, your group id, and — when joining an
existing cluster — the IP of any live node. No Go toolchain needed; the release
ships a prebuilt binary. Tested on **Ubuntu 22.04+**.

```bash
curl -fsSL https://raw.githubusercontent.com/shellylittleant/MeshCDN/main/scripts/quick-install.sh \
  | sudo bash -s -- \
      --bot-token=<your_telegram_bot_token> \
      --group-id=<your_telegram_group_id>
```

It resolves the latest release, verifies the download against the release's
`SHA256SUMS`, installs OpenResty and both systemd units, and generates the
self-signed fallback certificate.

Then, in your Telegram group:

```
/menu                                                  # main menu
/w domain https://example.com:443 https://1.2.3.4:443  # register a domain
/w ssl example.com -                                   # auto Let's Encrypt
/v domain example.com -                                # inspect it
```

That's it — `example.com` → `1.2.3.4:443` is now served with auto-renewing SSL.

> **Telegram setup**: create a bot via [@BotFather](https://t.me/BotFather),
> disable its privacy mode (or make it a group admin) so it can read commands,
> create a group, add the bot, and get the group ID. See
> [docs/V4-DESIGN.md](docs/V4-DESIGN.md) for details.

### Adding a second node

On the new machine, point it at any existing peer using the same one-command flow:

```bash
curl -fsSL https://raw.githubusercontent.com/shellylittleant/MeshCDN/main/scripts/quick-install.sh \
  | sudo bash -s -- \
      --bot-token=<token> \
      --group-id=<group_id> \
      --peer=<any-live-node-ip>
```

Same command as above plus `--peer`; that flag is what switches it from
"start a new cluster" to "join this one". Any live peer works as the
introducer — the bot node does not need to be up.

The new node authenticates with a shared secret derived from
`sha256(group_id + bot_token)`, pulls the full config, and joins the mesh.

To move an already-installed node to a newer build, use `upgrade-node.sh`
instead — it backs up the current binary and rolls back if the agent does not
come back up:

```bash
curl -fsSL https://raw.githubusercontent.com/shellylittleant/MeshCDN/main/scripts/upgrade-node.sh \
  | sudo bash
```

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
/w domain https://example.com:443 https://1.2.3.4:443   # add domain → origin
/w ssl example.com -                                     # issue Let's Encrypt cert
/w sslfile example.com -                                 # upload your own cert (+ attach files)
/w cache img-7d patterns=*.jpg,*.png ttl=604800          # define a cache object
/w bind https://example.com:443 cache:img-7d             # bind the object to a domain
/v export - -                                            # export full config (as a file)
/v status - -                                            # node status
```

Run the same commands from any node's CLI:

```bash
sudo cdn-agent exec "/w domain https://example.com:443 https://1.2.3.4:443"
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

New in v4.4.0:

- **Bilingual interface (English / 中文).** `/en` and `/cn` switch the language
  of menus, help, status output, traffic reports and error categories. The
  setting lives in `cluster_meta` and rides the normal config stream, so it
  survives a bot-role transfer — whichever node ends up polling Telegram speaks
  the language you chose — and replays from the snapshot after an upgrade.
  *Command syntax is never translated:* `/w domain …` is identical in both
  languages, because the command language is the product and the terminal is
  only a shell over it.
- **Bare-verb shortcuts in Telegram.** `/help`, `/sync`, `/status`, `/nodes`,
  `/stats`, `/export`, `/upgrade`, `/en`, `/cn` now work without the
  four-segment form. V4-DESIGN listed these all along but only `/menu` was ever
  implemented; every other bare verb failed with "command must have 4 segments".
  Expansion happens in the bot layer, so the core grammar stays strict and the
  CLI still takes the full form.

New in v4.3.2:

- **Removing a node now actually removes it, everywhere.** A snapshot states
  the whole peer set, but replaying one could only ever *add*
  (`/w internal peer-add`) — so `/w internal peer-remove` took effect only on
  the node that ran it. Every other node kept the stale entry, and the next
  time the node that ran the removal pulled a snapshot from one of them, the
  entry came back. The removal appeared to succeed and silently undid itself.
  Snapshot import now reconciles membership — both `peers.json` and the `peers`
  mirror table — against what the snapshot declares. A snapshot that declares
  no peers is treated as saying nothing about membership rather than as "the
  cluster is empty", and a node never removes itself from its own peer list.

New in v4.3.1:

- **Config generation is no longer destroyed by concurrent syncs.** Every peer
  reporting a higher `config_version` triggered its own snapshot pull, and the
  heartbeat pings all peers concurrently — so on a large cluster one round
  behind launched a pull per peer, each wiping and rebuilding the same nginx
  directory at once. One would delete `nginx.conf` and then fail to remove
  `servers/` ("directory not empty") because another was still writing into
  it, leaving the node with no config and every later `nginx -t` failing.
  Pulls are now single-flight, and config generation renders into a staging
  directory and publishes file by file, so a failed or overlapping run leaves
  the working config untouched. The symptom scales with cluster size: rare at
  3 nodes, near-certain at 28.

New in v4.3.0:

- **Verified, reversible cluster upgrades** — `/v upgrade` now polls peer
  heartbeats for the new `program_version` and reports "N/M confirmed +
  failure list" instead of acknowledging at dispatch time. Each peer backs up
  its previous binary and is supervised by an independent `systemd-run`
  watchdog that restores it if the restart is unhealthy. (Rollback protection
  applies from the *next* upgrade onward, since the watchdog ships in the
  binary already running on the receiving node.)
- **Per-node traffic statistics** — `/v stats [domain] [24h|7d]` over a new
  `logs.db`, aggregated per `(domain, minute, status)` from an incremental
  tail of nginx's JSON stats log. Collection sits outside the request path;
  7-day retention.
- **Cluster cache purge** — `/v cache - purge-all` clears every node and
  reloads; read-shaped, so it does not consume a config version.
- **Working User-Agent defense** — `ua=` regexes bound to a server merge into
  a single `access_by_lua_block`, with parse-time rejection of invalid,
  control-character, and catastrophic-backtracking patterns.
- **Multi-SAN certificate renewal** — renewal now uses `{Subject} ∪ SAN`, so
  a multi-domain certificate is no longer silently reduced to its CN.
- **Bot role transfer** — `/v target <ip>` moves the Telegram-facing role via
  a replicated `bot_node_ip` override (takes effect on the target's next
  agent restart).
- Stricter-wins merging for `size=` limits; closed parameter key sets on all
  rule objects (unknown keys now error instead of being dropped silently).

Not yet rebuilt from v3.x (planned):

- Rule templates (`#name` references) — superseded by the object/bind model
- Smart origin routing (ordered failover paths through peers)
- Bulk origin replacement, node-level redirects
- Cross-node statistics aggregation (`/v stats` is per-node today)
- `geo=` GeoIP blocking and `cc=` sliding-window protection (keys reserved;
  currently return an explicit "not implemented" error)

---

## Documentation

| Document | Topic |
|---|---|
| [Product Specification (PDF)](docs/MeshCDN-v4.3.0-Product-Specification.pdf) | Design philosophy, architecture, and the full feature catalogue — the best place to start if you want to understand *why* the system is shaped this way |
| [docs/V4-DESIGN.md](docs/V4-DESIGN.md) | The V4 "constitution": architecture, command grammar, schema, design rationale |
| [docs/AI-PRIMER.md](docs/AI-PRIMER.md) | Condensed overview of the system — start here if you're an AI tool or a new contributor |

---

## Building from source manually

`scripts/install.sh` builds automatically when no pre-built binary is present,
so end-users normally never run these commands. They're here for developers:

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