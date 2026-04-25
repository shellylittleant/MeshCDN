# Mesh Protocol

This document describes the wire-level protocol nodes use to coordinate. It's a reference for contributors and people debugging cluster issues.

If you only want to deploy MeshCDN, you don't need to read this.

---

## Overview

All inter-node traffic is HTTP+JSON over a single port (default `:9443`). There's no service mesh, no gRPC, no message queue. Just HTTP requests with bearer token auth.

Why this choice:
- **Debuggable**: `curl` works for any endpoint
- **Firewall-friendly**: a single port is easier to reason about
- **No long-lived connections**: each peer-to-peer call is independent
- **Simple to upgrade**: adding endpoints is just adding handlers

The downside is some overhead per call. For a CDN cluster of O(10) nodes with O(100) commands per day, this is negligible.

---

## Authentication

### Shared secret derivation

```
auth_token = sha256(group_id + bot_token)
```

Both `group_id` (Telegram group, a negative integer) and `bot_token` (Telegram bot token) are known to all peers. The hash is one-way — neither input is recoverable from the token.

### Header

Every request to authenticated endpoints includes:

```
Authorization: Bearer <auth_token>
```

Endpoints that don't require auth (intentionally) are:
- `/mesh/auth` — the bootstrap handshake (uses its own challenge/response)
- `/mesh/bootstrap` — public install script (no secrets in response)

### Failure mode

Missing or wrong bearer token → HTTP 401. Logged on the server side.

---

## Endpoints

### `POST /mesh/auth`

The bootstrap handshake. A new node uses this to join the cluster.

#### Request

```http
POST /mesh/auth HTTP/1.1
Host: <introducer>:9443
Content-Type: application/json

{
  "node_id": "newnode-abc123",
  "ip": "5.6.7.8",
  "port": 9443,
  "secret": "<sha256 of group_id+bot_token>"
}
```

#### Response

```json
{
  "ok": true,
  "join_order": 3,
  "peers": [
    {"node_id":"node1-...","ip":"1.2.3.4","port":9443,"join_order":1},
    {"node_id":"node2-...","ip":"5.6.7.9","port":9443,"join_order":2}
  ],
  "config_export": "/w domain https://example.com 443 https://1.2.3.4:443\n..."
}
```

The new node:
1. Stores the peer list locally
2. Replays each command in `config_export` against its local DB
3. Generates nginx config from the populated DB
4. Now functions as a full peer

The introducer:
1. Adds the new node to its peer list
2. Broadcasts an `Addition` notification to existing peers
3. Existing peers update their local peer lists

### `POST /mesh/exec`

Execute a command on a peer. This is how live broadcasts and on-demand commands work.

#### Request

```http
POST /mesh/exec HTTP/1.1
Host: <peer>:9443
Authorization: Bearer <auth_token>
Content-Type: application/json

{
  "node_id": "<sender-node-id>",
  "command": "/w domain https://a.com 443 https://1.2.3.4:443"
}
```

#### Response

```json
{
  "ok": true,
  "result": "✅ <output of the command>"
}
```

The receiving node:
1. Verifies the sender is in its peer list (or returns "unknown peer")
2. Logs the broadcast (`收到广播: <command> from <sender>`)
3. Validates the command (rejects deprecated formats — see below)
4. Executes locally via the same code path as a CLI invocation
5. Returns the result

The command is **not** re-broadcast — that would create infinite loops. Each command is broadcast once by its originator.

#### Special command prefixes

```
/internal/cert-install-v2 <domain>|<issuer>|<sans>|<not_after>|<cert_b64>|<key_b64>
/internal/cert-install <domain> <cert_pem>|||<key_pem>     # legacy
/internal/cert-delete <domain> <cert_id>
/internal/ssl-apply <domain>
/internal/ai-query <sql_query>
/internal/remove-peer <node_id>
/internal/relay-upgrade <target_ip:port>
```

These are "internal" — not exposed to users — and are used for cross-node operations like cert sync and the chain-relay upgrade mechanism.

### `POST /mesh/ping`

Heartbeat. Sent every minute by every node to every other node.

#### Request

```http
POST /mesh/ping HTTP/1.1
Authorization: Bearer <auth_token>
Content-Type: application/json

{
  "node_id": "<sender>",
  "cluster_version": 5,
  "routing_version": 12,
  "policy_version": 8
}
```

#### Response

```json
{
  "ok": true,
  "cluster_version": 5,
  "routing_version": 12,
  "policy_version": 8
}
```

#### Reconciliation

If sender and receiver have different version numbers for any of the three streams:

- **Higher version wins**: the side with the higher version pushes its config for that stream to the other side
- **Push uses `/mesh/exec`**: the high-version side replays the relevant subset of commands

In practice this catches missed broadcasts. Normal operation has all peers at the same versions, so heartbeats are no-ops.

#### Three-stream version split

```
cluster_version    incremented on peer add/remove (mesh membership)
routing_version    incremented on /w domain, /w port, /w ssl
policy_version     incremented on /w cache, /w defense, /w header, /w redirect
```

This split means a flurry of `/w cache` commands doesn't invalidate routing reconciliation, and vice versa. Reduces unnecessary sync churn.

### `GET /mesh/peers`

Returns the current peer list. Used internally and exposed for debugging.

```http
GET /mesh/peers HTTP/1.1
Authorization: Bearer <auth_token>
```

```json
[
  {
    "node_id": "node1-abc",
    "ip": "1.2.3.4",
    "port": 9443,
    "join_order": 1,
    "status": "online",
    "last_seen": "2026-04-22T12:00:00Z"
  },
  ...
]
```

### `GET /mesh/export`

Returns the full config-as-commands export. Equivalent to running `/v export` locally.

```http
GET /mesh/export HTTP/1.1
Authorization: Bearer <auth_token>
```

Response is plaintext (one command per line):

```
/w domain https://a.com 443 https://1.2.3.4:443
/w cache a.com *.jpg 604800
/w ssl a.com
...
```

### `POST /mesh/takeover`

Move the Bot role to this node.

```http
POST /mesh/takeover HTTP/1.1
Authorization: Bearer <auth_token>

{}
```

The receiving node:
1. Marks itself as the new Bot node
2. Broadcasts the change to all peers
3. Starts polling Telegram

### `GET /mesh/bootstrap`

**Special endpoint — does not require auth.**

Returns a shell script that installs MeshCDN on a new machine. Used by:

```bash
curl -s http://<peer>:9443/mesh/bootstrap | sudo bash -s -- <peer-ip> <token> <group-id>
```

The returned script:
1. Installs OpenResty if not present
2. Calls `/mesh/download` (auth-required) to fetch the binary
3. Verifies the binary's sha256
4. Writes the systemd unit, config.json
5. Starts the service

Because this endpoint serves up an install script with no auth, it's deliberately simple — it doesn't reveal anything sensitive. The auth-protected `/mesh/download` is what actually has the binary.

### `GET /mesh/download`

Returns the agent binary (binary octet stream).

```http
GET /mesh/download HTTP/1.1
Authorization: Bearer <auth_token>
```

Used during cluster-wide upgrades and during new-node bootstrap. The binary is held in memory by the running agent and served on demand.

---

## Broadcast pattern

When any node executes a `/w` or `/d` command:

```
   [User in Telegram] sends "/w domain ..."
              ↓
        [Bot node receives]
              ↓
     [Local execution succeeds]
              ↓
     [Local broadcast to peers]
   ┌──────────┴───────────┐
   ↓          ↓           ↓
 Peer A    Peer B       Peer C
   ↓          ↓           ↓
[execute] [execute] [execute]
   ↓          ↓           ↓
[respond] [respond] [respond]
              ↓
   [Originator counts successes]
              ↓
   [Replies to user: "✅ N/M nodes synced"]
```

Key properties:
- **One broadcast wave per user command**. Peers don't re-broadcast.
- **Best-effort**: if some peers fail or are unreachable, the originator just notes it. Heartbeat reconciliation will fix it.
- **Order-preserving per pair**: each (sender, receiver) pair has a serial connection, so commands arrive in send order.
- **Not order-preserving across pairs**: peer A might receive command 1 before peer B does. For idempotent commands (which all `/w` commands are designed to be), this is fine.

---

## Validation: rejecting old formats

The receiving side runs commands through a validator before execution. The validator's job is to reject commands from peers running incompatible older versions, to prevent corruption.

Current rejection rules (as of v3.1):
- `/w port <port> <origin>` (v2.x format) — accepted but a no-op (informational, since port protocol is now derived from `/w domain`)
- Commands with empty arguments — rejected
- Internal commands not in the known prefix list — rejected

The validator is conservative: it errs on the side of accepting things that look reasonable, and only rejects clearly broken or malicious-looking input.

---

## Failure handling

### Peer unreachable during broadcast

The originator times out after 5 seconds per peer. The reply to the user will note "X/Y nodes synced". The unreachable peer's missed update will be reconciled by heartbeat (within 1-3 minutes).

### Heartbeat fails

If a peer fails 3 consecutive heartbeats (≥3 minutes silent):
- It's marked `offline` in the peer list
- If it was the Bot node, automatic drift moves the role
- It's not removed from the peer list — it might come back

### Network partition

If the cluster splits into two groups that can each reach themselves but not each other:
- Each group continues serving traffic
- Each group's commands are local-only until reconnection
- On reconnection, version vectors diverge, and reconciliation pushes both ways

This is **not** a strongly consistent system. It's eventually consistent. If both halves of a partition issue conflicting commands (e.g. one says `/w cache a.com *.jpg 100`, the other says `/w cache a.com *.jpg 200`), the last write wins on each side and you'll have a divergence to resolve manually after reconnection.

We accept this trade-off. The alternative (consensus protocols, distributed locks, etc.) would add significant complexity for very rare scenarios. CDN config is mostly written by humans, who don't issue conflicting commands at the same time on different network partitions.

---

## Future protocol changes

Possible v3.2+ changes (not yet committed):

- **mTLS for mesh communication**: replace bearer token with client certificates. Already partially supported (the `/mesh/exec` payload could be signed), just not deployed.
- **gRPC migration**: would give us streaming and bidirectional channels, useful for `/v stats` of large clusters. Not a priority.
- **Compression**: large export payloads (cluster with hundreds of domains) could benefit from gzip. Cheap to add when needed.

These will go through normal proposal review before being adopted.

---

## Debugging

### Manually invoke endpoints

```bash
# Get peer list (replace TOKEN with sha256(group_id + bot_token))
curl -H "Authorization: Bearer $TOKEN" http://<peer>:9443/mesh/peers

# Trigger an export
curl -H "Authorization: Bearer $TOKEN" http://<peer>:9443/mesh/export
```

### Compute the auth token

```bash
GROUP_ID=$(sudo grep -o '"group_id":[[:space:]]*-?[0-9]*' /etc/meshcdn/config.json | awk -F: '{print $2}' | tr -d ' ')
BOT_TOKEN=$(sudo grep -o '"bot_token":[[:space:]]*"[^"]*"' /etc/meshcdn/config.json | sed 's/.*"\([^"]*\)"/\1/')
TOKEN=$(echo -n "${GROUP_ID}${BOT_TOKEN}" | sha256sum | awk '{print $1}')
echo "$TOKEN"
```

### Watch broadcasts in real time

```bash
sudo journalctl -u meshcdn -f | grep "广播\|broadcast"
```

### Force reconciliation

```bash
sudo cdn-agent exec "/sync"
```

This forces the local node to push its full config to all peers.
