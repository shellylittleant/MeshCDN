# MeshCDN V4 — AI Primer

**Read this first. Everything else is implementation detail.**

> Once you've read this file, an AI tool has enough context to work on the
> project. Read this at the start of every session by default. The real design
> document is `V4-DESIGN.md`; this file is a condensed extract.
>
> (Documentation is in English, but you can interact in any language — an LLM
> assistant replies in whatever language you write in.)

---

## What the project is

MeshCDN is a **self-hosted distributed CDN**. Multiple VPS nodes form a cluster
via a mesh protocol, controlled by a Telegram bot. Each node runs OpenResty +
cdn-agent. A single Go binary, roughly 5 MB.

---

## Six design principles (every decision traces back here)

1. **Equal peers** — All nodes hold identical configuration. The "master" is
   merely whichever node currently talks to the bot.
2. **Commands are configuration** — Configuration is a sequence of commands.
   Exporting is templating; replaying is recovery.
3. **Precision is priority** — All match-type rules are ordered automatically by
   precision; no manual weights.
4. **Heartbeat sync** — Nodes probe each other, cross-check, and converge
   automatically.
5. **Transactional consistency** — A batch is the atomic unit. The version
   counter only commits to successful commands; failed commands do not exist at
   the protocol level.
6. **The program's boundary = the information boundary** — The program informs;
   the human decides. Alerting is an obligation; fallback is not. The only
   exceptions are the bare minimum needed to keep the system bootable and
   reachable.

---

## Three subsystems (architectural skeleton)

```
Skeleton  →  /etc/meshcdn/persistent/  +  /etc/meshcdn/runtime/
             Two-directory scheme. On upgrade, runtime/ is wiped,
             persistent/ is kept.

Muscle    →  internal/command/
             Strict four-segment commands. All external input passes here.

Blood     →  internal/mesh/  +  internal/cert/renew/
             Background loops: heartbeat / certificate scan / event stream.
```

---

## Command system (memorize this)

### Strict four-segment form

```
/<verb> <type> <scope> <params>
```

Every segment must be present. Use `-` as a placeholder for an empty slot. No
exceptions.

- **verb**: `w` (write) / `d` (delete) / `v` (view)
- **type**: `domain` / `ssl` / `sslfile` / `cache` / `defense` / `redirect` /
  `header` / `bind` / cluster metadata / ...
- **scope**: depends on the type (domain / IP / object name / `-`)
- **params**: a `key=value` sequence, or `-`

### w/d mirror symmetry

```
write:  /w cache img-7d patterns=*.jpg ttl=604800
delete: /d cache img-7d patterns=*.jpg ttl=604800   ← same text, prefix only differs
```

Export = dump in-memory state as a sequence of `w` commands. Delete = change the
corresponding `w` to `d`.

### Command classes A / B / C

**Class A (direct commands, scope = a real business object)**
```
/w domain   <host:port>    <origin>            register a domain
/w ssl      <domain/IP>    <action or ->       request a certificate
/w sslfile  <domain/IP>    <option or ->       upload a certificate
```

**Class B (object commands, scope = object name, no extent)**
```
/w cache    <object-name>  <key=value, one line>   define a cache object
/w defense  <object-name>  <key=value, one line>   define a defense object
/w redirect <object-name>  <key=value, one line>   define a redirect object
/w header   <object-name>  <key=value, one line>   define a header object
```

**Class C (binding commands)**
```
/w bind     <domain/IP>    <object-type>:<object-name>
```

### Cluster metadata queries (view only)
```
/v export   -              -                    export the whole cluster config
/v status   -              -                    this node's status
/v stats    -              -                    traffic statistics
/v nodes    [- | <peer-ip>] -                   peer list / detail
```

### System actions (standalone verbs, not part of w/d/v)
```
/sync                                            trigger a sync proactively
/target  <peer-ip>                               move the bot role
/upgrade                                         trigger a cluster upgrade
/menu /help                                      menu / help
/confirm <ID>                                    second-step confirmation
```

### The `-` placeholder

`-` uniformly means "fallback / default / no constraint" in every field:

- domain field `-` = any host (lowest priority)
- origin field `-` = this node serves itself (default page)
- view command scope `-` = list everything
- view command params `-` = no detail filter

---

## Key design points

### Node enrollment
`secret = sha256(group_id + bot_token)`. Any peer can accept an enrollment.

### Sync
A single monotonically increasing `config_version` integer. Snapshots are
full-replace, no deltas. A lagging node pulls from the peer with the lowest RTT.
Proactive push + heartbeat fallback run in parallel.

### Certificates
- Sources: LE > upload > self-signed (precision order)
- IP = domain (no special-casing)
- Issuing node: `candidates(D) = nodes whose IP is in D's DNS A records;
  responsible = candidates[hash(D) % len]`
- Once selected, a cert is not swapped until it expires; on expiry it is
  immediately re-selected by the precision algorithm
- Self-signed certs are generated during install.sh, valid 100 years, and do not
  participate in sync

### Upgrade
`runtime/` is wiped entirely; only the 4 `persistent/` items survive:
`identity.json` / `peers.json` / `certs/` / `snapshot.cmd`.

### Global port-protocol uniqueness
Each port has exactly one protocol binding across the whole cluster. A conflict →
warning + `/confirm`.

### Multi-binding precision matching
When a domain binds multiple objects, matching follows precision rules; on a
precision tie, the "stricter" field value wins (TTL takes the minimum, booleans
OR together). The matching engine is delegated to nginx — cdn-agent does not
implement a runtime matcher.

---

## Project layout (quick reference)

```
cmd/cdn-agent/main.go               entry point
internal/
├── identity/      persistent/identity.json
├── peers/         persistent/peers.json
├── snapshot/      persistent/snapshot.cmd
├── db/            runtime/config.db
├── command/       ★ Muscle layer
│   ├── types.go         core types + Handler interface + parser
│   ├── executor.go      batch transactions
│   ├── portproto.go     global port-protocol table logic
│   └── handlers/        one file per type
├── nginx/         generates runtime/nginx/*
├── cert/          certificate subsystem
│   ├── store.go         certs/ + manifest.json
│   ├── selector.go      §3.6 selection algorithm
│   ├── selfsign.go      self-signed generation
│   ├── acme/            ACME client
│   └── renew/           ★ renewal loop (scanner.go + worker.go)
├── mesh/          ★ Blood layer (client.go, coordinator.go, server.go, upgrade.go)
├── bot/           Telegram interface (thin layer)
├── cli/           cdn-agent exec (thin layer)
├── logs/          access log collection
└── version/       version + embedded source (version/source/)
```

---

## Engineering discipline

1. **Package boundary = physical boundary**. One internal package maps to one
   physical file/directory/concept.
2. **One type per file under handlers/**. A new feature = a new handler.
3. **The build must go through source-snapshot**. `make build` copies the source
   into `internal/version/source/` and embeds it into the binary.
   `cdn-agent dump-source` can restore it. **A binary without self-contained
   source must not be released.**
4. **Handlers do no I/O**. Validate is pure logic; Write/Delete go through the
   transaction; side effects are reported to the executor via `Effects` for
   unified handling.
5. **Within a batch, failures are skipped and execution continues**, but only
   successful commands advance the version counter.

---

## Documents to consult

- `V4-DESIGN.md` — the full design document (consult as needed)
- `internal/command/types.go` — all core type definitions of the command system
  (with detailed comments)
- This file — the overview (read by default at the start of each session)

---

**Core creed**: the foundation matters; configuration is commands; commands are
four segments; nodes are equal; the program's boundary is the information boundary.
