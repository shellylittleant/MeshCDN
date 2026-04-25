# Configuration & Command Reference

MeshCDN's entire configuration is expressed through a small command language. This document lists every command.

The same commands work in three places:

- **Telegram group** — the default frontend; messages prefixed with `/`
- **Local CLI** — `sudo cdn-agent exec "/w domain ..."` on any node
- **Mesh broadcast** — peers sending commands to each other (internal)

For the *why* behind the design, see [architecture.md](architecture.md).

---

## Syntax conventions

| Symbol | Meaning | Example |
|---|---|---|
| Space | Field separator | `/w domain a.com 80 http://1.2.3.4:80` |
| Comma | Multi-value within one field | `a.com,b.com` or `80,443` |
| Newline | Multiple commands at once | (paste multiple lines into Telegram) |
| `-` | "this machine's IP" (dash = self) | `/w domain - 80,443 http://...` |

The dash placeholder is special: when used in the *domain* field of `/w domain`, it's treated as a port-fallback (the rule applies to any IP on this node). When used in *target URL* fields like redirects, it expands per-node to that node's own IP.

---

## Three core verbs

```
/w <type> <args>     # write (create or update)
/d <type> <args>     # delete
/v <type> [args]     # view (read or export)
```

Resource types: `domain`, `cache`, `redirect`, `header`, `ssl`, `defense`, `block`, `ruleset`, `route`, `replace`, `node-redirect`.

---

## /w domain — Domain & port management

### Format

```
/w domain <proto>://<name> <ports> [<origin-url>]
```

- `<proto>` is `http` or `https` — required in v3.0+
- `<name>` is the domain or `-` for port fallback
- `<ports>` is a comma-separated port list, or a single port
- `<origin-url>` is `http://addr:port` or `https://addr:port`, or `-` to serve a welcome page

### Examples

```
# Basic HTTPS domain
/w domain https://example.com 443 https://1.2.3.4:443

# Same domain on multiple ports, same origin
/w domain https://example.com 443,8443 https://1.2.3.4:443

# Different origins per port
/w domain https://example.com 443 https://primary:443
/w domain https://example.com 8443 https://secondary:8443

# Port fallback (any request on port 317 goes to this origin)
/w domain http://- 317 http://1.2.3.4:317

# Multiple domains sharing config
/w domain https://a.com,b.com 443 https://shared-origin:443

# Welcome page on this port (no origin proxying)
/w domain https://- 443 -

# Reference rule template (see ruleset section below)
/w domain https://example.com 443 https://origin:443 #img-cache #force-https
```

### Subcommands

```
/w domain a.com #+template-name      # add a template reference
/w domain a.com #-template-name      # remove a template reference
/w domain a.com #clear               # clear all template references
/w domain a.com comment "memo text"  # set a comment
```

---

## /w port — DEPRECATED

Removed in v3.0. Port protocol is now derived from the `<proto>://` prefix in `/w domain`.

If you see `/w port` in old documentation or scripts, replace it:

```
# Old (v2.x):
/w port 443

# New (v3.0+):
/w domain https://- 443 -    # port 443 is HTTPS, no specific origin
```

---

## /w cache — Cache rules

### Format

```
/w cache <domain> <pattern> <cdn-ttl> [<browser-ttl>] [hsts]
```

- `<pattern>` is a path pattern (see below) or `preset <name>`
- `<cdn-ttl>` is seconds; 0 = don't cache
- `<browser-ttl>` is optional; defaults to CDN TTL; `-` skips
- `hsts` adds `Strict-Transport-Security` header

### Pattern types

```
*.jpg,*.png            extension match
/api                   path prefix
= /exact-path          exact match
^~ /static/            prefix match (stop searching)
~ ^/api/v[0-9]+/       regex
```

### Presets

```
/w cache a.com preset static       # 7-day cache for static assets
/w cache a.com preset dynamic      # short TTL, follow origin Cache-Control
/w cache a.com preset mixed        # balanced
/w cache a.com preset aggressive   # very long TTL (use carefully)
```

### Examples

```
/w cache a.com *.jpg,*.png 604800              # 7-day CDN cache
/w cache a.com *.css 86400 2592000 hsts        # 1d CDN, 30d browser, with HSTS
/w cache a.com /api 0                          # don't cache API
/d cache a.com                                 # delete all cache rules for domain
/d cache a.com purge                           # purge cache files (rules unchanged)
```

---

## /w ssl — SSL certificates

### Format

```
/w ssl <domain>                # auto Let's Encrypt
/w ssl <domain> selfsign       # self-signed
/w ssl -                       # IP cert for this node
/w ssl <domain> @<node-ip>     # apply specifically on a peer node
```

### Behavior

- Reuses an existing valid cert if one is found (avoids LE rate limits)
- Falls back to ZeroSSL if Let's Encrypt rate-limits
- Falls back to self-signed if both fail (with a warning)
- Auto-renewal runs every 6 hours; renews if remaining lifetime < 7 days (3 days for IP certs)

### Health check

```
/v ssl                 # list all certs
/v ssl health          # state machine view (live / renewed_not_live / etc.)
/v ssl <domain>        # detail for one cert
/v ssl all             # cross-cluster view
```

### Delete

```
/d ssl <domain>
```

This stops auto-renewal and removes cert files. The domain stays configured but reverts to whatever fallback cert is available.

---

## /w redirect — Redirect rules

### Format

```
/w redirect <domain> <from-path> <to-url> <status>
```

### Examples

```
/w redirect a.com /old /new 301
/w redirect a.com / https://canonical.example.com 301
/w redirect a.com /api http://-:8080/ 302    # - = this node's IP
```

`<status>` is the HTTP status code (301, 302, 307, 308).

---

## /w header — Header modification

### Format

```
/w header <domain> <direction> <op> <name> [<value>]
```

- `<direction>` is `request` (toward origin) or `response` (toward client)
- `<op>` is `add` or `del`

### Examples

```
/w header a.com response add X-Frame-Options DENY
/w header a.com response add Strict-Transport-Security "max-age=31536000"
/w header a.com response del Server
/w header a.com request add X-Real-IP $remote_addr
/d header a.com                                # clear all
```

Variables: nginx variables like `$remote_addr` work in values.

---

## /w defense — Application-layer defense

### Format

```
/w defense <action> <type:value> <duration> <port> <domain>
```

- `<action>` is `block` or `allow`
- `<type:value>` is one of seven prefixes
- `<duration>` is seconds, or `-` for permanent
- `<port>` is a port, or `-` for all
- `<domain>` is a domain, or `-` for global

### Type prefixes

| Prefix | Function | Backend | Status |
|---|---|---|---|
| `ip:` | IP / CIDR block | nginx `deny`/`allow` | ✓ |
| `ref:` | Referer check | nginx `valid_referers` | ✓ |
| `size:` | Request size limit | nginx `client_max_body_size` | ✓ |
| `rate:` | Rate limiting | nginx `limit_req_zone` | ✓ |
| `geo:` | Country block | GeoLite2 + Lua | planned |
| `ua:` | User-Agent filter | Lua `access_by_lua` | planned |
| `cc:` | CC protection (sliding window auto-ban) | Lua | planned |

### Examples

```
/w defense block ip:1.2.3.4 - - -             # block IP globally, permanent
/w defense block ip:1.2.3.0/24 86400 - a.com   # block subnet for 1 day on a.com
/w defense allow ip:10.0.0.1 - - -             # whitelist (allow takes precedence)
/w defense block rate:100 - - a.com            # 100 req/s rate limit on a.com
/w defense block size:10m - - -                # block requests larger than 10 MB globally
/w defense allow ref:*.a.com,none - - a.com    # only allow Referer matching *.a.com or empty

/d defense ip:1.2.3.4
/d defense a.com           # delete all rules for a.com
/d defense all             # nuke all rules
```

### `/v block` — Quick blocked-IP view

Shorthand for `/v defense ip-only`. Lists only IP-type rules.

---

## /w ruleset — Rule templates (v3.1)

Define a set of rules once, reference from any number of domains using `#name` syntax.

### Five template types

```
cache             # /w cache rule sets
defense_block     # /w defense block ... rule sets
defense_allow     # /w defense allow ... rule sets
header            # /w header rule sets
redirect          # /w redirect rule sets
```

### Format

```
/w ruleset <name> <type> <rule-content>
```

For multi-rule templates, the content can span multiple lines (in Telegram, use Shift+Enter for newline; in CLI, use a multi-line string).

### Examples

```
# Cache template
/w ruleset img-cache cache *.jpg,*.png,*.gif 604800

# Header template (multi-rule)
/w ruleset sec-headers header
response add X-Frame-Options DENY
response add Referrer-Policy strict-origin-when-cross-origin
response del Server

# Redirect template
/w ruleset force-https redirect / https://{host}{uri} 301

# Defense template
/w ruleset bad-bots defense_block
ua:.*BadBot.*
ip:1.2.3.4
```

Variables available in `redirect`: `{host}`, `{uri}` (expand to nginx `$host`, `$request_uri`).

### Apply to a domain

```
/w domain https://a.com 443 https://origin:443 #img-cache #sec-headers #force-https

# Or modify an existing domain
/w domain a.com #+img-cache       # add reference
/w domain a.com #-img-cache       # remove
/w domain a.com #clear            # remove all
```

### View / delete

```
/v ruleset                   # list all templates
/v ruleset <name>            # detail (rules + which domains reference it)
/d ruleset <name>            # delete (asks for confirmation if referenced)
```

---

## /w route — Smart origin routing (v3.1)

Configure ordered failover paths for an origin: try direct first, then via peer node A, then via peer node B, etc.

### Format

```
/w route <origin-url> <path-list>
```

- `<origin-url>` is the origin (must be IP-based; domain origins not allowed)
- `<path-list>` is comma-separated: `direct`, peer IP, or combinations

### Examples

```
# Just direct (default behavior)
/w route https://1.2.3.4:443 direct

# Try peer 47.82.82.159 first, then direct
/w route https://1.2.3.4:443 47.82.82.159,direct

# Multiple fallback peers
/w route https://1.2.3.4:443 47.82.82.159,124.71.45.16,direct
```

### View / delete

```
/v route                           # list all configured paths
/v route <origin-url>              # detail for one origin
/v route matrix                    # node connectivity matrix
/d route <origin-url>              # remove (revert to direct-only)
```

### Why IP-only

If you specify a domain as origin (`https://origin.example.com`), nginx tries to resolve it at startup — and if DNS fails, the entire nginx config refuses to load. To prevent this footgun, MeshCDN rejects non-IP origins for routing rules. If you need a domain origin, resolve it manually first (`dig +short`).

---

## /w replace — Bulk source replacement

### Format

```
/w replace <old-ip> <new-ip>
```

Updates all occurrences of `<old-ip>` in the `domains` and `domain_ports` tables to `<new-ip>`. Useful when you migrate an origin server.

### Example

```
/w replace 1.2.3.4 5.6.7.8
```

This updates all domains pointing at `1.2.3.4` to point at `5.6.7.8` instead. Reload happens automatically.

---

## /v export — Export configuration

```
/v export
```

Outputs the entire current configuration as a sequence of `/w` commands. Replay these commands to recreate the configuration on a fresh cluster.

Properties of the export:
- Only `https` ports are exported (HTTP is the default and doesn't need explicit declaration)
- Domains using `-` are exported with `-` preserved (each node will substitute its own IP)
- SSL entries are deduplicated by domain
- Expired defense rules are skipped

---

## Management commands

### `/menu`

Open the main menu. Categories:
- 🌐 Domains
- 🛡 Rules
- 🖥 Nodes
- 🔀 Routing & Network
- 🤖 AI Assistant

Plus utility buttons: 📤 Export, 🔄 Sync, ⬆️ Upgrade, ℹ️ Help.

### `/sync`

Force full configuration sync from the local node to all peers. Useful if you suspect drift.

### `/target <peer-ip>`

Move the Bot role to a specific peer. Use this to manually move Telegram control between nodes.

### `/upgrade`

Triggers a cluster-wide upgrade. The current Bot node uploads its `cdn-agent` binary to all peers.

### `/help`

Shows command reference.

---

## /v — View commands

```
/v domain                  # all domains
/v domain <name>           # detail
/v ssl                     # all certs
/v ssl <name>              # detail
/v ssl health              # state machine view
/v ssl all                 # cross-cluster
/v cache <domain>          # cache rules for domain
/v redirect <domain>       # redirect rules
/v header <domain>         # header rules
/v defense [<domain>]      # defense rules
/v block                   # blocked IPs
/v ruleset                 # all templates
/v ruleset <name>          # template detail
/v route                   # origin paths
/v route <origin>          # path detail
/v route matrix            # connectivity matrix
/v nodes                   # peer list
/v nodes <ip>              # peer detail
/v status                  # local node status
/v stats                   # access statistics
/v logs [<domain>]         # access log query
/v ai                      # recent AI conversations
/v export                  # full configuration as commands
```

---

## Configuration file (config.json)

The agent reads `/etc/meshcdn/config.json`:

```json
{
  "node_id": "hostname-abc123",
  "bot_token": "1234567890:ABC...",
  "group_id": -1001234567890,
  "peer_addr": "1.2.3.4",
  "mesh_port": 9443,
  "ai_provider": "openai",
  "ai_openai_key": "sk-...",
  "ai_deepseek_key": "sk-..."
}
```

| Field | Required | Notes |
|---|---|---|
| `node_id` | yes (auto-generated on install) | unique cluster identifier |
| `bot_token` | yes | Telegram bot token |
| `group_id` | yes | Telegram group ID (negative number) |
| `peer_addr` | for non-first nodes | IP of an existing peer to authenticate to |
| `mesh_port` | no (default 9443) | port for inter-node mesh communication |
| `ai_provider` | optional | `openai` or `deepseek` |
| `ai_openai_key` | if `ai_provider=openai` | OpenAI-compatible API key |
| `ai_deepseek_key` | if `ai_provider=deepseek` | DeepSeek API key |

This file is preserved across upgrades. Treat it as you'd treat private keys — anyone with this file controls your cluster.

---

## CLI usage

```bash
# Execute any command locally (same syntax as Telegram, without /menu/help/etc.)
sudo cdn-agent exec "/w domain https://a.com 443 https://1.2.3.4:443"

# Multi-line input
sudo cdn-agent exec "$(cat <<'EOF'
/w ruleset img-cache cache *.jpg,*.png 604800
/w ruleset sec-headers header
response add X-Frame-Options DENY
EOF
)"

# Take over the Bot role from CLI
sudo cdn-agent takeover

# List backups
sudo cdn-agent restore --list

# Restore from a backup
sudo systemctl stop meshcdn
sudo cdn-agent restore --backup=20260424-150000
sudo systemctl start meshcdn
```

The CLI is the recommended path for scripting and emergency recovery. It bypasses Telegram entirely.
