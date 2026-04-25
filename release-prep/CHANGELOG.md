# Changelog

All notable changes to MeshCDN will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it reaches v1.0.

---

## [Unreleased]

### Planned
- Generic DNS provider integration (Cloudflare, Aliyun, AWS Route53)
- Live latency probing for smart routing v2
- mTLS for mesh communication
- English-first documentation across all packages

---

## [3.1.0-alpha11] — 2026-04

### Fixed
- CLI command executor now accepts v2.11-format `/w domain` broadcasts (without protocol prefix), inferring HTTP/HTTPS from port number
- New nodes joining a cluster with mixed-format historical commands no longer drop port-fallback configuration

### Notes
This was a critical fix for cross-version cluster deployments where a v2.x bot node broadcasts to a v3.1 new node.

---

## [3.1.0-alpha10] — 2026-04

### Fixed
- AI mention detection no longer calls `GetMe()` on every incoming message; uses cached username/ID from startup. Eliminates a class of "bot looks frozen" failures caused by transient Telegram API slowness
- `/v ssl health` query corrected (was using wrong column name `created_at` instead of `timestamp`)

### Added
- Entry-point logging in the message router for easier diagnosis of "did the message arrive?" issues
- Failure-fallback in reply functions: if Telegram rejects a message (HTML parse error etc.), automatically send a stripped-tag fallback so users see *something*

---

## [3.1.0-alpha9] — 2026-04

### Added — UI refactor (Phase C)
- Five-category main menu replacing the old flat layout: Domains / Rules / Nodes / Routing&Network / AI Assistant
- Domain detail page now aggregates everything (ports, certs, rules, ruleset references, origin paths, timeouts) on one screen
- Node detail page (`/v nodes <ip>`) with system info for the local node
- Connectivity matrix view (`/v route matrix`)
- AI assistant menu showing recent patrol records

---

## [3.1.0-alpha8] — 2026-04

### Added — Certificate hardening (Phase B)
- Certificate state machine: `live` / `pending_reload` / `renewed_not_live` / `stale` / `unknown`
- `/v ssl health` command for at-a-glance certificate health overview
- `manifest.json` as authoritative cert metadata source, with history
- Auto-reconcile: stuck `renewed_not_live` certs auto-promote to `live` when reload succeeds
- Telegram alert when renewal succeeds but reload fails (so live certs and disk certs diverge)
- Pre-upgrade automatic backup of `config.db`, `manifest.json`, `peers.json`
- New `cdn-agent restore --list` and `cdn-agent restore --backup=<timestamp>` for one-command rollback

---

## [3.1.0-alpha6 → alpha7]

### Changed
- **Rule template reference symbol changed from `@name` to `#name`** to avoid conflicts with Telegram's user-mention semantics. Previous form caused some messages with multiple `@xxx` references to silently fail
- All rule template syntax updated: `#+name` (add), `#-name` (remove), `#clear` (clear all references)

### Note
The `@<IP>` syntax for cross-node addressing (e.g. `/w ssl @192.0.2.1`) is preserved — pure IP addresses are not affected by Telegram's mention parser.

---

## [3.1.0-alpha1 → alpha5]

### Added — Rule templates and origin routing (Phase A)
- New `/w ruleset <name> <type> <rules>` command for defining reusable rule templates: cache, defense_block, defense_allow, header, redirect
- New `/w route <origin> <path-list>` command for configuring per-origin failover paths (direct + relay-via-peer)
- nginx `upstream` blocks now auto-generated with `backup` directive for failover

### Fixed
- Global panic recovery in command router prevents one bad command from killing the entire bot session
- Reload failures now broadcast a Telegram alert instead of being logged silently
- Route preferences with non-IP origins are rejected (would have caused nginx to refuse to start due to startup-time DNS resolution)
- Several edge cases in the message reply fallback path

---

## [3.0.0] — 2026-01

### Added — v3.0 major release
- Domain commands now require explicit protocol prefix: `/w domain https://example.com 443 https://1.2.3.4:443`
- IP certificates managed independently per node
- Domain certificates synced cluster-wide
- Welcome page on ports without configured origins
- Simplified mesh exec with bearer token authentication

### Removed
- The `/w port` command (port protocol now derived from `/w domain` prefix)
- The `port_defaults` table (data migrated into `domains` + `domain_ports` with `name='-'`)

---

## [2.11.x] — 2025-Q3 to 2025-Q4

### Added
- Equal-peer mesh architecture (no master, no leader election)
- Bot-node drift: any peer can become the bot node; Telegram polling moves automatically
- Three-stream version vectors (cluster / routing / policy) for sync reconciliation
- Peer authentication via shared secret derived from `sha256(group_id + bot_token)`
- Static binary build with embedded SQLite

### Note
Earlier versions were internally numbered but not formally released. The v2.11.x line is what most production deployments are running today.

---

## [Pre-2.x] — Initial development

Initial development cycle. Architecture explored, mesh protocol prototyped, command surface designed. Not formally versioned.

---

[Unreleased]: https://github.com/<org>/meshcdn/compare/v3.1.0-alpha11...HEAD
[3.1.0-alpha11]: https://github.com/<org>/meshcdn/releases/tag/v3.1.0-alpha11
