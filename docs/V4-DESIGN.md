# MeshCDN V4 Design Document

**Version**: v0.5 (2026-05) — reconciled with the shipped v4.0.20 implementation
**Status**: implemented; this document now describes the system as built
**Purpose**: the "constitution" of the V4 rewrite — the authority for all implementation decisions

**Main changes in v0.5** (post-implementation reconciliation):
- Added **§13 AI assistant subsystem** — shipped in v4.0.17–v4.0.20 but absent from the original v0.4 design. The constitution now governs it.
- Added **§14 file-based config I/O** — the Telegram export/import attachment workflow added in v4.0.19.
- Revised **§3.7** to address multi-SAN certificate renewal (a gap the original renewal spec did not cover).
- Marked **§12** as historical: it was the pre-implementation build plan; the work is now done.
- Source-tree listings in §A.1 / §A.3 corrected to match the actual module layout (e.g. `command/types.go` holds the parser; `mesh/` is `client.go` / `coordinator.go` / `server.go` / `upgrade.go`; embedded source lives in `internal/version/source/`).

**Earlier (v0.4, 2026-04-27)**:
- Added **§A Overview of the three subsystems** (Skeleton / Muscle / Blood)
- Command system formally fixed: strict four-segment form + mirror symmetry + object/binding two-layer abstraction
- Added §8 the complete command geometry (command classes A/B/C, primary keys, merge rules, global port-protocol uniqueness)
- Database schema outline, error-code taxonomy, and the confirmation UX nailed down together

---

# §A Overview of the three subsystems

V4's overall architecture is split into three subsystems by **static / semi-static / dynamic** lifecycle. Each maps to a different lifecycle of code and files.

## A.1 Skeleton

The things that sit still on disk. **Two-directory scheme.**

```
/etc/meshcdn/
├── persistent/                       ← kept across upgrades
│   ├── identity.json                 node identity + bot token + group_id
│   ├── peers.json                    cluster node IP list
│   ├── certs/                        all certificates + manifest.json
│   └── snapshot.cmd                  current effective config export (with version header)
│
└── runtime/                          ← wiped on upgrade, rebuilt from persistent/
    ├── config.db                     SQLite main database
    ├── logs.db                       access logs
    ├── nginx/                        generated nginx config
    ├── welcome/                      default page (overwritten every upgrade)
    ├── challenges/                   ACME validation temp files
    ├── cache/                        nginx cache
    ├── logs/                         raw access logs
    └── tmp/                          general temp files
```

The physical upgrade procedure: **delete the entire runtime/ directory, stop the service, replace the binary, start the service, and the agent automatically rebuilds runtime/ from persistent/**.

If a future V5 substantially changes the architecture, as long as V5 still understands these 4 kinds of persistent files, it can upgrade smoothly from V4.

Source organization (project directory tree):

```
meshcdn/
├── cmd/cdn-agent/main.go             program entry point
├── internal/                         all business logic (Go internal packages are not importable externally)
│   ├── identity/                     manages persistent/identity.json
│   ├── peers/                        manages persistent/peers.json
│   ├── snapshot/                     manages persistent/snapshot.cmd
│   ├── db/                           manages runtime/config.db
│   ├── command/                      ★ Muscle system
│   │   ├── types.go                  Command / Handler / Effects / Rule / error codes / parser
│   │   ├── executor.go               batch transactions
│   │   ├── portproto.go              global port-protocol table logic
│   │   └── handlers/                 one handler per type
│   ├── nginx/                        generates runtime/nginx/*
│   ├── cert/                         certificate subsystem
│   │   ├── store.go                  certs/ + manifest.json
│   │   ├── selector.go               §3.6 selection algorithm
│   │   ├── selfsign.go               self-signed generation
│   │   ├── acme/                     ACME client
│   │   └── renew/                    ★ renewal loop (scanner.go + worker.go)
│   ├── mesh/                         ★ Blood circulatory system
│   │   ├── server.go                 HTTP+JSON endpoints
│   │   ├── client.go                 outbound peer calls
│   │   ├── coordinator.go            heartbeat + version reconciliation + bot role
│   │   └── upgrade.go                cluster-wide upgrade distribution
│   ├── bot/                          Telegram interface
│   ├── cli/                          cdn-agent exec
│   ├── logs/                         access log collection
│   └── version/                      version + embedded source
│       └── source/                   source copy filled in at build time
├── scripts/install.sh, bootstrap.sh
├── docs/V4-DESIGN.md, AI-PRIMER.md
├── go.mod, go.sum, Makefile
```

**Package boundary = physical boundary**: `internal/identity/` manages `persistent/identity.json`, `internal/snapshot/` manages `persistent/snapshot.cmd`. Changing one file's format touches only one package.

## A.2 Muscle

The `internal/command/` package and its handlers. **The rules that translate input into behavior.**

The core is the **strict four-segment form**:

```
verb  type   scope   params
─────────────────────────────
/w   domain   ...     ...
/d   domain   ...     ...
/v   domain   ...     ...
```

A new feature = a new `handlers/<name>.go` implementing the `Handler` interface and registered in the Registry. **On upgrade this package necessarily changes (new features), but the interface does not** — this is the physical embodiment of "upgrades don't affect command logic".

See §8 for details.

## A.3 Blood

The three automatic loops at runtime. **Continuously flowing.**

```
Loop 1 ─ heartbeat (every 1 minute)
  internal/mesh/coordinator.go (+ client.go for the outbound calls)
  ping all peers, exchange (config_version, program_version, peer_count)
  measured RTT written to the local peer table
  detect a lagging version → trigger a pull-sync task

Loop 2 ─ certificate scan (every 6 hours)
  internal/cert/renew/scanner.go
  scan all certificates; those with < 3 days remaining enter renewal (§3.7)

Loop 3 ─ event-stream processing (continuous)
  internal/mesh/coordinator.go + server.go
  receive event-stream messages (alerts, challenge sharing, bot-transfer notices)
  dispatch to the corresponding handler
```

**Loops never call each other directly — they communicate only via channels + the db.** If any one loop hangs or dies, the others are unaffected.

---

# §0 Design philosophy

V4 keeps four core principles from v3.1 and adds two:

1. **Equal peers**: all nodes hold identical configuration. The so-called "master" is just the node currently handling Bot commands; it has nothing special about it.

2. **Commands are configuration**: configuration has no other format. Configuration is a sequence of commands. Exporting is templating; replaying is recovery.

3. **Precision is priority**: all match-type rules are ordered automatically by precision; no manual weights.

4. **Heartbeat sync**: nodes probe each other, cross-check, and configuration converges automatically.

5. **Transactional consistency**: a batch is the atomic unit; the version counter only commits to successful commands; failed commands do not exist at the protocol level.

6. **The program's boundary = the information boundary**: the program informs, the human decides. Alerting is an obligation; fallback is not. The only exceptions are the bare minimum to keep the system bootable / reachable.

V4 explicitly abandons concepts from v3.1:
- Rule templates (ruleset) → replaced by the object/binding two-layer abstraction (§8.4)
- Domain groups → batch commands handle bulk scenarios
- Three-stream version counters (cluster/routing/policy) → merged into a single `config_version`
- "Don't auto-swap once selected" → conflicts with precision-first, abolished
- Differentiation between IP and domain certs → IP = domain, one rule set
- Excessive fallback → the boot-time self-signed check on upgrade and the agent's first-start re-issue are both removed

---

# §1 Sync mechanism

## 1.1 Heartbeat protocol

Every node sends a heartbeat to all peers every minute. The heartbeat exchanges:

```
config_version    : int    current config version
program_version   : str    current program version
peer_known_count  : int    total peers this node knows
```

Side effect: measured RTT is written to the local peer table.

## 1.2 Proactive push

After a batch executes, the Bot node immediately sends `notify-version` (just the new version number) to all peers. On receipt a peer compares its local version and pulls if behind.

`/sync` is a manual trigger for a proactive push.

Proactive push and heartbeat fallback **coexist** — proactive push may drop packets; the heartbeat is the eventual-consistency guarantee.

## 1.3 Pull model (snapshot)

When a lagging node decides to pull:
1. Filter the local peer table for peers with version ≥ self + 1
2. Within that set, pick the one with the lowest RTT
3. Pull the full config snapshot (command list + target version)
4. Locally clear + replay + bump version (in the same SQLite transaction)

A lagging node does not depend on the bot node; it can pull from any newer peer.

## 1.4 Version counter

`config_version` is a monotonically increasing integer. Each batch (with a single command as a special case) bumps it by 1. The write and the bump are in the same transaction:

```sql
BEGIN;
  -- apply commands to db
  UPDATE cluster_meta SET config_version = config_version + 1;
COMMIT;
```

## 1.5 Bandwidth probe for program upgrades

A config snapshot is small; low RTT basically means fast. A program upgrade (5 MB binary) is worth a small probe: on upgrade, send a 1 MB probe HEAD request to candidate peers and download from the one with the highest bandwidth.

## 1.6 Two-stream protocol

V4's mesh protocol has two message classes:

- **Config-sync class**: enters the version stream, guaranteed delivery, eventually consistent
- **Event-notification class**: does not enter the version stream, transient messages (alerts, challenge sharing, bot transfer)

Loss in the event-notification class is acceptable, backstopped by higher-level mechanisms.

---

# §2 Transaction semantics

## 2.1 The batch is the atomic unit

A user sends N commands at once (newline-separated); the bot executes them in order:
- A failed command is **skipped and execution continues**
- After all run, **only successful commands enter the new version**
- Failure details are returned to the user as an execution report
- A batch of N = one version increment

## 2.2 Failed commands never reach the protocol layer

The version other nodes pull always commits only to the slice of "confirmed successful" commands.

## 2.3 Consistency guarantee

V4 is an **eventually consistent** system. Normal sync latency < 1 second (proactive push); abnormal < 2 minutes (two rounds of heartbeat fallback).

---

# §3 Certificate mechanism

## 3.1 Design tenets

- **IP and domain are identical** — same commands, issuance, sync, and selection algorithm
- **Three sources**: LE / upload / self-signed — metadata labels, identical processing
- **Source is precision**: LE > upload > self-signed

Factual premise: Let's Encrypt has officially issued IP certificates (short-lived, 6 days) since July 2025. V4's design is based on this premise.

## 3.2 Issuing-node selection

```
candidates(D) = [node P : D's DNS resolution includes P's IP]
responsible(D) = candidates(D)[hash(D) % len(candidates(D))]
```

DNS bounds the candidate set; hash allocates within it. A DNS change → the candidate set changes → hash re-allocates.

## 3.3 HTTP-01 validation (V4 default)

Before the responsible node X issues, it broadcasts the ACME challenge token via the event stream to all nodes in the candidate set. Every candidate node's nginx reads from the shared challenge directory.

## 3.4 DNS-01 validation (V4.1 target)

Not implemented in V4.

## 3.5 Certificate sync

After an LE / upload certificate is stored, it is broadcast cluster-wide via the config-sync stream. **Self-signed certs do not participate in sync.**

## 3.6 Certificate selection algorithm

```
To select the current effective certificate for a domain / IP endpoint X:
  candidates = all non-expired certificates covering X

  sort (high to low):
    1. source: LE > upload > self-signed
    2. within the same source: latest expiry first

  behavior after selection:
    - do not swap the current selection until it expires
    - on expiry / invalidation, immediately re-select by the algorithm
```

## 3.7 Renewal procedure

Scan every 6 hours; for a currently selected certificate X with < 3 days remaining:

```
Step 0: classify
  X.source ∈ {LE, self-signed}  → Step 1
  X.source == upload            → Step 2

Step 1: auto-renew
  LE → the responsible node runs ACME (async task)
  self-signed → regenerate locally
  success → replace X
  failure → Step 2

Step 2: find a replacement
  among candidates, a non-self-signed cert with not_after > X.not_after
  found → switch
  not found → Step 3

Step 3: alert
  push to Telegram via the event stream, including the raw ACME error
  de-duplicated over 24 hours

— There is no Step 4. Expiry is handled naturally by the §3.6 selection algorithm.
```

Implementation: an async task queue (in-process memory queue), not persisted. After a restart the next scan rediscovers automatically.

## 3.7.1 Multi-SAN renewal (v0.5)

A certificate may cover multiple names (a SAN list), either because it was
issued for several domains or because a user uploaded a multi-SAN certificate.
The renewal in §3.7 **must preserve the certificate's full SAN list**, not just
its Common Name.

```
On renewing cert X:
  names = X.san            ← the COMPLETE SAN list, not just X.subject (CN)
  re-issue / regenerate for `names`
  the replacement cert must cover every name X covered
```

Rationale: re-issuing for the CN alone silently "slims" a multi-name certificate
down to a single name on its next renewal, which then fails to serve the dropped
SANs. The selection algorithm (§3.6) would not catch this — it only checks
coverage of the endpoint being selected, and the renewed cert still covers that
one endpoint.

> **Known limitation (v4.0.20)**: the shipped renewal path re-issues from the
> certificate subject (CN) only; it does **not** yet preserve the full SAN list.
> Externally uploaded multi-SAN certificates are therefore at risk of being
> slimmed to their CN on the first auto-renewal (~6 days for LE). Storage and
> selection already support multi-SAN; only the renewal issuance step needs the
> fix above. Until fixed, multi-SAN certs that must not be slimmed should be of
> source `upload` and re-uploaded before expiry, or excluded from auto-renewal.

## 3.8 Self-signed certificates

- **When generated**: during install.sh. **install.sh is the sole responsible party** — the agent does not re-issue at startup
- **Validity**: 100 years (one-time)
- **Renewal**: regenerated locally when the 3-day threshold fires
- **Sync**: does not participate
- **Storage**: `/etc/meshcdn/persistent/certs/`, differing only by source label
- **Priority**: lowest, as a fallback

## 3.9 Certificate directory and naming

Named by content fingerprint:

```
/etc/meshcdn/persistent/certs/<sha256-prefix>.crt
/etc/meshcdn/persistent/certs/<sha256-prefix>.key
/etc/meshcdn/persistent/certs/manifest.json
```

`manifest.json` is the metadata index:

```json
{
  "certificates": {
    "<sha256-prefix>": {
      "subject": "a.com",
      "san": ["a.com"],
      "source": "le|upload|self",
      "issuer": "Let's Encrypt R3",
      "not_before": "...",
      "not_after": "...",
      "fingerprint_sha256": "...",
      "selected_for": ["a.com"]
    }
  }
}
```

---

# §4 Upgrade and persistence

## 4.1 Cross-upgrade retention list

Only 4 items:

| Path | Content |
|---|---|
| `persistent/identity.json` | node identity |
| `persistent/peers.json` | cluster node IP list |
| `persistent/certs/` | all certificates + manifest |
| `persistent/snapshot.cmd` | current config export |

The entire `runtime/` directory is deleted and rebuilt on every upgrade.

## 4.2 snapshot file format

```
# version: 53
# exported: 2026-04-27T12:34:56Z
# program: v4.0.0
/w domain https://a.com:443 https://1.2.3.4:443
/w cache  img-7d            patterns=*.jpg,*.png ttl=604800
/w bind   a.com             cache:img-7d
/w ssl    a.com             -
...
```

Re-exported and overwritten after every successful batch (write to a temp file, then rename).

## 4.3 Upgrade boot procedure

```
1. Read the 4 persistent/ items
2. Restore config_version into memory from the snapshot file header
3. Rebuild the database schema
4. Replay snapshot commands into the db
5. Generate nginx config + start OpenResty
6. Join the heartbeat network
7. Compare versions with neighbors; pull the latest config if needed
```

There is no "self-signed-check fallback" step.

## 4.4 Release artifact structure and source self-containment

**Any released binary must be able to recover its corresponding source.**

Implementation: belt and suspenders

1. The release tarball contains a source copy (a separate directory)
2. The binary embeds the source (Go embed)
3. The commit hash is compiled into the binary

Release artifact structure:

```
meshcdn-v4.0.0-linux-amd64.tar.gz
├── cdn-agent               binary (with embedded source)
├── install.sh
├── source/                 flat source (without .git/)
└── VERSION                 commit hash + build time
```

New commands:

```bash
cdn-agent --version           # output includes the commit hash
cdn-agent dump-source         # defaults to ./meshcdn-source-<commit>/
```

Build discipline: CI makes "package source + embed into binary" a mandatory pre-release step. **A binary without self-contained source must not be released.**

---

# §5 Node management

## 5.1 Enrollment

Any peer can accept an enrollment. A new node N enrolls with an existing node P:

1. N submits `secret = sha256(group_id + bot_token)`
2. After verifying, P initiates an "add peer" config change (enters the version-sync stream)
3. All nodes in the cluster converge by version
4. P returns the current snapshot to N
5. N replays the snapshot + joins the heartbeat network

## 5.2 Peer list folded into the config stream

A peer joining / leaving = one protocol command `/internal/peer-add` / `/internal/peer-remove`, +1 version.

## 5.3 Bot-node determination

- The first node installed defaults to bot
- `/target <ip>` transfers the bot role
- Bot unreachable: **purely manual takeover**. SSH into any surviving node and run takeover

No automatic drift / automatic alerting / split-brain protection.

---

# §6 IP endpoint access behavior

Per the **IP = domain** principle:

```
access https://1.2.3.4:443
  ↓
Has 1.2.3.4 been explicitly registered via /w domain?
  ├── no  → default server + self-signed + CDN default page (200)
  └── yes → use that IP endpoint's config (select cert per §3.6)
```

---

# §7 CDN default page

- Built-in static HTML, released to `runtime/welcome/index.html` during install.sh
- Not customizable (V4 first version)
- Not synced
- Not in the cross-upgrade retention list (overwritten every upgrade)
- Status code 200

---

# §8 Command system (Muscle layer)

## 8.1 Strict four-segment form

Every command is **strictly four segments**:

```
/<verb> <type> <scope> <params>
```

- Every segment must be present; use `-` as a placeholder for an empty slot
- The parser always expects four segments, no exceptions

Format:

```
verb     ::= w | d | v
type     ::= a resource-type string (domain / ssl / cache / defense / bind / ...)
scope    ::= depends on the type
params   ::= depends on the type; may be - or a key=value sequence
```

`-` uniformly means "fallback / default / no constraint" in every field.

## 8.2 Mirror symmetry (w/d)

w and d are mirror operations. **Export = dump in-memory state as a sequence of w commands; delete = change the corresponding w to d.**

```
write:  /w cache img-7d patterns=*.jpg ttl=604800
delete: /d cache img-7d patterns=*.jpg ttl=604800
```

w/d share the Parse and Validate paths; they diverge only at Execute.

**Primary-key determination**: each type defines which fields constitute the primary key. Two w commands with the same primary key are treated as two writes to the same rule (overwrite semantics). A d command matches by primary key; non-key fields in a d command may or may not be written, and are only validated for form.

## 8.3 Command classification (classes A/B/C + cluster metadata + system actions)

### Class A: direct commands (scope = a real business object)

```
/w domain   <host:port>       <origin>            register a domain
/w ssl      <domain/IP>       <action or ->       request a certificate
/w sslfile  <domain/IP>       <option or ->       upload a certificate
```

### Class B: object commands (scope = object name, no extent)

```
/w cache    <object-name>     <key=value, one line>   define a cache object
/w defense  <object-name>     <key=value, one line>   define a defense object
/w redirect <object-name>     <key=value, one line>   define a redirect object
/w header   <object-name>     <key=value, one line>   define a header object
```

### Class C: binding commands

```
/w bind     <domain/IP>       <object-type>:<object-name>
```

### Cluster metadata queries (view only)

```
/v export   -                 -                   export the whole cluster config
/v status   -                 -                   this node's status
/v stats    -                 -                   traffic statistics
/v nodes    [- | <peer-ip>]   -                   peer list / detail
```

### System action commands (standalone verbs)

```
/sync                                              trigger a sync proactively
/target  <peer-ip>                                 move the bot role
/upgrade                                           trigger a cluster upgrade
/menu                                              show the menu
/help                                              show help
/confirm <ID>                                      confirm the previous dangerous op
```

## 8.4 Object/binding model (replacing v3.1's ruleset)

Defining objects (class B) and binding relationships (class C) are decoupled:

```
/w cache img-7d  patterns=*.jpg,*.png ttl=604800
/w cache html-1h patterns=*.html      ttl=3600
/w bind a.com    cache:img-7d
/w bind a.com    cache:html-1h
```

- One object can be bound by many domains
- One domain can bind many objects
- Deleting a referenced object → warning + `/confirm` for a cascading delete

## 8.5 Precision-first matching for multiple bindings

When a domain binds multiple objects, matching follows precision rules:

```
template A: patterns=*.jpg,*.png  ttl=604800
template B: patterns=zhangsan.jpg ttl=86400

a.com binds A and B:
  request zhangsan.jpg → B wins (exact match)   → cache 1 day
  request lisi.jpg     → A wins (wildcard)       → cache 7 days
```

Precision rules: exact > prefix > regex > wildcard; a longer CIDR prefix has higher precision.

**Matching is delegated to nginx**: cdn-agent generates nginx location blocks in precision order (exact `location =`, prefix `location ^~`, regex `location ~*`, wildcard fallback), and nginx's own matching engine performs precision-first matching. **cdn-agent does not implement a runtime matching engine.**

## 8.6 Same-precision merge rules

When same-precision patterns conflict across different objects, **take the "stricter" field value**:

| Field type | Merge rule |
|---|---|
| TTL | take the minimum |
| HSTS / other booleans (enabling = stricter) | OR |
| Action (block/allow) | block wins |
| Other strictness fields | the stricter one wins |

No error, no user choice, no ordering by bind sequence — **merge directly**.

## 8.7 Global port-protocol uniqueness

Each port has exactly one protocol binding across the whole cluster.

Implementation: runtime/config.db keeps a `port_protocols(port, protocol)` table. Each `/w domain` command implicitly updates this table based on its host:port segment.

Conflict handling:

```
/w domain http://b.com:443 ...    # but 443 is already bound as https
   ↓
return ErrPortConflict with a PendingConfirmation
   ↓
"⚠️ Port 443 is already bound as https. Reply /confirm <ID> to force it to http"
   ↓
user replies /confirm <ID> → port switches → syncs cluster-wide
```

`/v export` does not explicitly output port bindings (they are implicitly rebuilt from domain commands).

## 8.8 Confirmation UX (PendingConfirmation)

Triggers:
- Port-protocol conflict
- Cascading delete (deleting a referenced object)
- Possible future dangerous operations

Mechanism:
- Before a dangerous command executes, generate a `PendingConfirmation` with a short ID
- Stored **in process memory** (not persisted, lost on restart)
- Notify the user: "⚠️ reason. Reply `/confirm <ID>` to continue"
- TTL = 5 minutes, auto-cleared on expiry
- The user replies `/confirm <ID>` → the original command is retrieved, marked "confirmed", and executed
- Restart / 5-minute expiry → the user just resends the command

CLI side:

```bash
cdn-agent exec "/w domain http://b.com:443 ..."
# Error: PORT_CONFLICT — 443 currently bound to https. Use --force to override.

cdn-agent exec --force "/w domain http://b.com:443 ..."
# ✅ Port 443 switched to http
```

---

# §9 Database schema outline

Main tables in `runtime/config.db`:

```sql
-- metadata
CREATE TABLE cluster_meta (
    config_version INTEGER NOT NULL DEFAULT 0,
    bot_node_ip    TEXT,
    program_version TEXT
);

-- class A rules (direct commands)
CREATE TABLE domains (
    id INTEGER PRIMARY KEY,
    scope TEXT NOT NULL UNIQUE,    -- "https://a.com:443"
    origin TEXT NOT NULL,          -- "https://1.2.3.4:443" or "-"
    config_version INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

-- global port-protocol table
CREATE TABLE port_protocols (
    port INTEGER PRIMARY KEY,
    protocol TEXT NOT NULL
);

-- class B rule objects (unified table, distinguished by the type field)
CREATE TABLE rule_objects (
    id INTEGER PRIMARY KEY,
    type TEXT NOT NULL,            -- "cache" / "defense" / "redirect" / "header"
    name TEXT NOT NULL,            -- object name
    params_text TEXT NOT NULL,     -- raw key=value text
    parsed_json TEXT NOT NULL,     -- structured JSON after the handler parses (for the nginx generator)
    config_version INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    UNIQUE(type, name)
);

-- class C binding relationships
CREATE TABLE bindings (
    id INTEGER PRIMARY KEY,
    scope TEXT NOT NULL,           -- "a.com" or IP
    object_type TEXT NOT NULL,
    object_name TEXT NOT NULL,
    config_version INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    UNIQUE(scope, object_type, object_name),
    FOREIGN KEY (object_type, object_name) REFERENCES rule_objects(type, name)
);

-- certificates (mirror of manifest.json; the db is a query-friendly copy)
CREATE TABLE certificates (
    fingerprint_prefix TEXT PRIMARY KEY,
    subject TEXT NOT NULL,
    source TEXT NOT NULL,          -- "le" / "upload" / "self"
    issuer TEXT,
    not_before TIMESTAMP,
    not_after TIMESTAMP,
    san_json TEXT NOT NULL         -- ["a.com", "*.a.com"]
);

-- peer list (mirror of peers.json)
CREATE TABLE peers (
    ip TEXT PRIMARY KEY,
    join_order INTEGER NOT NULL,
    last_seen_rtt_ms INTEGER,
    last_seen_at TIMESTAMP
);
```

`runtime/logs.db` is a separate database for access logs; its schema is refined when that step is implemented.

---

# §10 Error-code taxonomy

All command-system errors return a `CommandError` interface with a `Code()` short code:

| Code | Meaning |
|---|---|
| `BAD_FORMAT` | malformed command (not four segments, illegal verb) |
| `UNKNOWN_TYPE` | type not in the Registry |
| `BAD_PARAMS` | params failed to parse or value is illegal |
| `NOT_FOUND` | /d on a non-existent rule (informational, not necessarily fatal) |
| `CONFIRM_REQUIRED` | command is held, awaiting /confirm |
| `CONFIRM_EXPIRED` | /confirm came too late |
| `CONFIRM_UNKNOWN` | unknown /confirm ID |
| `PORT_CONFLICT` | port-protocol conflict without confirm |
| `CASCADE_BLOCKED` | deleting a referenced object |
| `INTERNAL` | bug |

The bot/cli layer decides the UX based on Code — the Telegram side can use emoji + friendly hints, the CLI side a non-zero exit code + stderr output.

---

# §11 Differences from v3.1

| Dimension | v3.1 | V4 |
|---|---|---|
| Command format | each type a different segment count | strict four-segment (verb + type + scope + params) |
| Command classification | flat | classes A/B/C + cluster metadata + system actions |
| Rule reuse | ruleset templates | object + binding two-layer abstraction |
| Precision-first | partial (domain layer) | full (domain/path/IP), engine delegated to nginx |
| Port protocol | per-domain | cluster-wide unified table + confirmation |
| Version counter | three streams | single config_version |
| Sync semantics | delta broadcast | snapshot pull model |
| Failed commands | enter the broadcast stream | never reach the protocol layer |
| Batch | per single command | the batch is the atomic unit |
| ruleset | first-class citizen | removed |
| IP vs domain | differentiated | fully unified |
| Cert sources | LE + self-signed IP (special) + upload | LE + upload + self-signed (unified) |
| Cert selection | no swap once selected | select best by precision + no swap while current is valid + immediate re-select on expiry |
| Self-signed | 6-day IP cert | install.sh-generated, 100 years |
| Fallback philosophy | layered defense | information boundary (the program informs, the human decides) |
| File naming | by domain/IP | by content fingerprint + manifest |
| Cross-upgrade retention | 4 items + db | 4 items (db rebuilt) |
| Persistence layering | many directories | two directories (persistent/runtime) |
| Bot unreachable | automatic drift | purely manual takeover |
| Peer list | separate Addition broadcast | folded into the config stream |
| Source recoverable | not guaranteed | mandatory (commit hash + embed) |
| AI assistant | read-only SQL question answering | natural-language → command suggestion, execute via confirm button (§13) |

---

# §12 Implementation priority order

> **Historical (v0.5 note)**: this was the pre-implementation build plan, in
> dependency order. All steps below are now complete in v4.0.20. It is retained
> as a record of how the build was sequenced, not as a description of current
> status. Two subsystems shipped beyond this plan — see §13 (AI assistant) and
> §14 (file-based config I/O).

In dependency order:

| Step | Content | Time | Milestone |
|---|---|---|---|
| 0 | Project skeleton (directory tree + Makefile + version + dump-source) | half a day | `cdn-agent --version` works |
| 1 | DB schema + command parsing + first handler (domain) + CLI exec | 2-3 days | `cdn-agent exec /w domain ...` writes + reads back |
| 2 | OpenResty generator + install.sh + self-signed fallback | 3-4 days | a single machine reverse-proxies real traffic |
| 3 | Full certificate subsystem (LE + upload + renewal) | 4-5 days | LE certs auto-issued and renewed |
| 4 | Mesh protocol skeleton + two-node sync | 5-7 days | two nodes sync config within seconds |
| 5 | Batch transactions + snapshot file + cluster upgrade | 3-4 days | rolling upgrade with no loss |
| 6 | Object/binding + precision generator + port-protocol table | 3-4 days | cache/defense objects work |
| 7 | Telegram bot interface | 2-3 days | v3.1 commands all run on V4 |

After step 3 (about 2 weeks) there is an independently deployable single-node V4. By step 6 the command system is fully implemented; step 7 wraps it up.

---

# §13 AI assistant subsystem (optional)

> Added post-v0.4. Shipped across v4.0.17–v4.0.20. The original design document
> did not anticipate this subsystem; this section brings it under the constitution.

## 13.1 What it is — and what it deliberately is not

The AI assistant is an **optional natural-language front-end to the command
system**. Its single job is to translate a user's natural-language request into
correct MeshCDN four-segment commands. It is **not** a database query engine and
does **not** read live cluster state.

This is a deliberate departure from the v3.x assistant, which ran read-only SQL
against a config replica to answer questions. The V4 assistant works the other
direction: it produces *commands*, never answers from a DB.

It honors principle 6 (the program informs, the human decides) in the strongest
form: **the AI only suggests; it never executes**. Suggested commands are wrapped
in a markdown code block tagged `command`; the bot extracts these blocks and
renders execute / cancel buttons. The user's tap is what executes — and that tap
runs through the exact same command path (§8) and transaction (§2) as any typed
command. Plain prose replies (no code block) are shown as-is.

```
user (@mention): "cache jpg and png on a.com for a week"
        ↓
LLM reply contains:  ```command
                     /w cache img-7d patterns=*.jpg,*.png ttl=604800
                     /w bind https://a.com:443 cache:img-7d
                     ```
        ↓
bot renders [✅ Execute] [❌ Cancel]
        ↓
user taps Execute → commands run through the normal executor → synced cluster-wide
```

## 13.2 Triggering

- An `@mention` of the bot in the Telegram group starts an AI conversation.
- A reply to a bot message continues the conversation.
- A message beginning with `/` is always treated as a direct command, never sent
  to the AI.

Non-Bot nodes never run the assistant (they have no Telegram client). The AI is
purely a Bot-node convenience layer on top of the command system.

## 13.3 Providers

Six providers are supported, all reached over HTTP:

```
openai      api.openai.com                          (OpenAI Chat Completions shape)
gemini      generativelanguage.googleapis.com       (Google OpenAI-compatible endpoint)
grok        api.x.ai
deepseek    api.deepseek.com
qwen        dashscope.aliyuncs.com/compatible-mode  (Alibaba OpenAI-compatible)
claude      api.anthropic.com/v1/messages           (native Messages shape)
```

Five of the six speak the OpenAI Chat Completions wire format and share a single
implementation (`internal/ai/openai_compatible.go`); each has a thin file that
only sets its base URL / auth header. Claude uses its native Messages shape
(`x-api-key` + `anthropic-version` header + top-level `system` + content blocks)
and has its own client. `assistant.go` holds the `NewProvider` factory and the
conversation dispatch; `prompt.go` holds the system prompt (the command-grammar
reference the LLM translates against).

## 13.4 Configuration

Per-provider API keys and per-provider model overrides live in
`persistent/identity.json`:

```json
{
  "ai_provider": "gemini",
  "gemini_api_key": "AIza...",
  "gemini_model": "gemini-2.5-flash-lite",
  "openai_api_key": "sk-...",
  "openai_model": "gpt-4o-mini"
}
```

- `ai_provider` selects the **active** provider. Unset → AI disabled.
- Each provider has its own key field and its own model field. An empty model
  field falls back to `DefaultModel(provider)`. Keys for inactive providers are
  retained, so switching providers does not lose credentials.

Configured via the command system (class B-style operation scope):

```
/w ai provider <name>            switch active provider (openai/gemini/grok/claude/deepseek/qwen)
/w ai key <key>                  set the key for the active provider
/w ai key <provider>:<key>       set the key for a specific provider
/w ai model <model>              set the model for the active provider
/w ai model <provider>:<model>   set the model for a specific provider
/d ai - -                        disable AI (clears ai_provider; keys kept)
/d ai key <provider>             clear one provider's key
/v ai - -                        show current AI config (keys masked)
```

## 13.5 Boundaries

- The assistant cannot bypass the command layer. It cannot execute, cannot reach
  the DB, cannot touch the mesh. It emits text; the user's button tap is the only
  thing that turns text into action.
- No patrol / autonomous health-check mode (the v3.x periodic patrol was dropped
  in V4). Any future autonomous behavior must be re-justified against principle 6.
- API keys are part of `identity.json` — treat them as secrets (the file already
  holds the bot token). They are not synced via the config stream.

---

# §14 File-based configuration I/O (optional)

> Added in v4.0.19; cert-upload routing refined in v4.0.20. A convenience layer
> over the snapshot model (§4.2) and the command system (§8).

## 14.1 Export

`/v export - -` returns the full cluster configuration as a downloadable text
file attachment (rather than only inline text). The file is the same
command-sequence format as `snapshot.cmd` (§4.2):

```
filename: meshcdn-export-v<config_version>-<UTC-timestamp>.txt
content:  one /w command per line, replayable verbatim
```

This makes "back up my config" and "move config to another cluster" a one-tap
operation, and keeps the export human-readable and diff-able.

## 14.2 Import

A user uploads a `.txt` file to the Telegram group. The bot parses it and shows a
**preview with a confirmation button** before anything runs — import is never
silent.

Parsing rules (`internal/bot/config_import.go`):

- Strip a UTF-8 BOM if present; normalize CRLF → LF.
- Skip blank lines and `#` comment lines.
- `/v` (view) lines are dropped with a count (a config file should not contain
  queries).
- `/w` and `/d` lines are collected into a batch.
- Malformed lines are reported, not silently ignored.

The collected batch is summarized ("N writes, M deletes, K skipped") and held
under a short import ID. The user taps:

```
[✅ Apply]  → runs the batch through the normal executor (§2 batch transaction,
              one version increment, failures skipped-and-reported)
[❌ Cancel] → discards the held batch
```

So import reuses the same atomic-batch semantics as any multi-line command paste;
the file upload is just another way to deliver the batch.

## 14.3 Disambiguation from certificate upload

Both certificate upload (§3, `/w sslfile`) and config import accept file uploads
over Telegram. They are disambiguated by **content sniffing** (v4.0.20), not by
filename:

- A file whose content contains a PEM block (`-----BEGIN ...`) is routed to the
  certificate-upload path regardless of caption or extension. Cert vs. key is
  decided by the PEM header (`CERTIFICATE` vs. `PRIVATE KEY`), and a cert/key
  pair is validated (`tls.X509KeyPair`) before storage. Encrypted private keys
  are rejected with guidance to decrypt first.
- A non-PEM text file with an empty caption (or `/import`) is routed to config
  import.

This makes both upload flows caption-optional: drag the file in and the bot
figures out what it is.

## 14.4 Size guard

Config import rejects files over 2 MB — a configuration export is virtually never
that large, so an oversized upload is treated as a wrong-file mistake.

---

# Appendix A: change history

- **v0.1** (2026-04-27): first version, protocol layer closed out
- **v0.2** (2026-04-27): cert mechanism + IP endpoint + default page finalized
- **v0.3** (2026-04-27): design philosophy adds principle 6; renewal simplified; two over-fallbacks removed; source self-containment added
- **v0.4** (2026-04-27): added the three-subsystem overview (Skeleton/Muscle/Blood); command system formally fixed (strict four segments + mirror symmetry + object/binding + precision-first + global port-protocol uniqueness + confirmation UX); database schema outline; error-code taxonomy
- **v0.5** (2026-05): post-implementation reconciliation with v4.0.20 — added §13 (AI assistant subsystem) and §14 (file-based config I/O), both shipped after v0.4 was written; revised §3.7 for multi-SAN renewal and recorded the current CN-only limitation; marked §12 as historical; corrected the source-tree listings to the actual module layout; added the AI-evolution row to §11