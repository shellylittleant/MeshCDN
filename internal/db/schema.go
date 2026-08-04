// Package db owns runtime/config.db — the SQLite database that holds the
// in-memory state of all rules.
//
// The database is REBUILDABLE: it lives under /etc/meshcdn/runtime/ which
// is wiped on every upgrade and rebuilt from persistent/snapshot.cmd. Do
// not store anything here that is not also derivable from snapshot.cmd
// or another persistent/ source.
//
// Schema design follows V4-DESIGN.md §9.
package db

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// Schema is the complete CREATE TABLE script for runtime/config.db.
// Idempotent (uses IF NOT EXISTS) so it can run on every boot.
const Schema = `
-- Cluster-level metadata. Single row.
CREATE TABLE IF NOT EXISTS cluster_meta (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    config_version  INTEGER NOT NULL DEFAULT 0,
    bot_node_ip     TEXT,
    program_version TEXT,
    language        TEXT
);
INSERT OR IGNORE INTO cluster_meta (id, config_version) VALUES (1, 0);

-- A class — direct rules where scope is a real business object.
-- One row per (host:port) endpoint.
CREATE TABLE IF NOT EXISTS domains (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    scope           TEXT NOT NULL UNIQUE,           -- canonical single-port scope: "https://a.com:443"
    display_scope   TEXT NOT NULL,                  -- user's original multi-port form: "https://a.com:443,8443"
    origin          TEXT NOT NULL,                  -- "https://1.2.3.4:443" or "-"
    config_version  INTEGER NOT NULL,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_domains_scope ON domains(scope);
-- idx_domains_display_scope is created by migrate() in Open(), because
-- CREATE INDEX would fail on old databases that don't yet have the
-- display_scope column.

-- Port-protocol global table. One row per port, cluster-wide invariant.
-- Per V4-DESIGN §8.7.
CREATE TABLE IF NOT EXISTS port_protocols (
    port      INTEGER PRIMARY KEY,
    protocol  TEXT NOT NULL CHECK (protocol IN ('http', 'https'))
);

-- B class — rule objects (cache/defense/redirect/header).
-- Unified table; type column distinguishes.
CREATE TABLE IF NOT EXISTS rule_objects (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    type            TEXT NOT NULL,                  -- "cache" / "defense" / ...
    name            TEXT NOT NULL,
    params_text     TEXT NOT NULL,                  -- raw "key=value ..." text
    parsed_json     TEXT NOT NULL,                  -- handler-parsed structure
    config_version  INTEGER NOT NULL,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(type, name)
);

-- C class — bindings between domain scopes and rule objects.
-- The "scope" column here MUST be the literal display_scope of an existing
-- row in the domains table (see V4-DESIGN: bind scope follows domain
-- display_scope verbatim, including multi-port lists like "https://a.com:443,8443").
CREATE TABLE IF NOT EXISTS bindings (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    scope           TEXT NOT NULL,                  -- equals domains.display_scope
    object_type     TEXT NOT NULL,
    object_name     TEXT NOT NULL,
    config_version  INTEGER NOT NULL,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(scope, object_type, object_name)
);
CREATE INDEX IF NOT EXISTS idx_bindings_scope ON bindings(scope);
CREATE INDEX IF NOT EXISTS idx_bindings_object ON bindings(object_type, object_name);

-- Certificates — query-friendly mirror of persistent/certs/manifest.json.
CREATE TABLE IF NOT EXISTS certificates (
    fingerprint_prefix  TEXT PRIMARY KEY,
    subject             TEXT NOT NULL,
    source              TEXT NOT NULL CHECK (source IN ('le', 'upload', 'self')),
    issuer              TEXT,
    not_before          TIMESTAMP,
    not_after           TIMESTAMP,
    san_json            TEXT NOT NULL                -- JSON array of SANs
);
CREATE INDEX IF NOT EXISTS idx_certs_subject ON certificates(subject);
CREATE INDEX IF NOT EXISTS idx_certs_not_after ON certificates(not_after);

-- Peers — mirror of persistent/peers.json plus runtime stats.
CREATE TABLE IF NOT EXISTS peers (
    ip                 TEXT PRIMARY KEY,
    join_order         INTEGER NOT NULL,
    last_seen_rtt_ms   INTEGER,
    last_seen_at       TIMESTAMP
);
`

// Open opens the SQLite database at path, creating it if necessary,
// and applies the schema.
//
// Caller is responsible for closing the returned *sql.DB.
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_foreign_keys=on", path)
	conn, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %s: %w", path, err)
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := conn.ExecContext(context.Background(), Schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return conn, nil
}

// migrate applies idempotent column additions to bring older databases
// (e.g. v4.0.12 and earlier) up to the current schema.
//
// SQLite's CREATE TABLE IF NOT EXISTS does NOT add new columns to an
// existing table — it only creates the table if missing. So we have to
// detect missing columns and ALTER TABLE manually.
func migrate(conn *sql.DB) error {
	ctx := context.Background()

	// v4.0.13: domains table gained `display_scope` column.
	hasDisplayScope, err := columnExists(ctx, conn, "domains", "display_scope")
	if err != nil {
		return err
	}
	if !hasDisplayScope {
		// Old DB: add the column.
		if _, err := conn.ExecContext(ctx,
			`ALTER TABLE domains ADD COLUMN display_scope TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add display_scope column: %w", err)
		}
		// Backfill: set display_scope = scope for any row where it's empty.
		// (Best-effort migration: pre-existing rows lose original multi-port
		// grouping, but each row is at least preserved.)
		if _, err := conn.ExecContext(ctx,
			`UPDATE domains SET display_scope = scope WHERE display_scope = ''`); err != nil {
			return fmt.Errorf("backfill display_scope: %w", err)
		}
	}
	// v4.4.0: cluster_meta gained `language` (interface language, synced
	// cluster-wide the same way bot_node_ip is).
	hasLanguage, err := columnExists(ctx, conn, "cluster_meta", "language")
	if err != nil {
		return err
	}
	if !hasLanguage {
		if _, err := conn.ExecContext(ctx,
			`ALTER TABLE cluster_meta ADD COLUMN language TEXT`); err != nil {
			return fmt.Errorf("add language column: %w", err)
		}
	}

	// Always create the index (idempotent via IF NOT EXISTS). Whether the
	// column was just added or existed all along, we need the index either way.
	if _, err := conn.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_domains_display_scope ON domains(display_scope)`); err != nil {
		return fmt.Errorf("create display_scope index: %w", err)
	}

	return nil
}

// columnExists checks PRAGMA table_info(table) for a named column.
func columnExists(ctx context.Context, conn *sql.DB, table, column string) (bool, error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// CurrentVersion reads cluster_meta.config_version. Returns 0 for fresh DBs.
func CurrentVersion(ctx context.Context, q Querier) (int64, error) {
	var v int64
	err := q.QueryRowContext(ctx, "SELECT config_version FROM cluster_meta WHERE id = 1").Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("read config_version: %w", err)
	}
	return v, nil
}

// BumpVersion increments cluster_meta.config_version by 1 within tx.
// Returns the new version.
func BumpVersion(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		"UPDATE cluster_meta SET config_version = config_version + 1 WHERE id = 1",
	); err != nil {
		return 0, fmt.Errorf("bump version: %w", err)
	}
	return CurrentVersion(ctx, tx)
}

// GetBotNodeIP reads cluster_meta.bot_node_ip. Empty string means "no explicit
// override" — callers then fall back to the join-order default (peers.BotNodeIP).
// See V4-DESIGN §5.3 (bot 节点确定 / 手动转移).
func GetBotNodeIP(ctx context.Context, q Querier) (string, error) {
	var ip sql.NullString
	err := q.QueryRowContext(ctx, "SELECT bot_node_ip FROM cluster_meta WHERE id = 1").Scan(&ip)
	if err != nil {
		return "", fmt.Errorf("read bot_node_ip: %w", err)
	}
	return ip.String, nil
}

// SetBotNodeIP sets (or, with ip=="", clears) the explicit bot-node override.
// This is a config mutation: it must run inside the batch tx so the version
// bump and snapshot export capture it and the cluster converges.
func SetBotNodeIP(ctx context.Context, tx *sql.Tx, ip string) error {
	var val interface{}
	if ip != "" {
		val = ip
	} // else nil → SQL NULL (clear override)
	if _, err := tx.ExecContext(ctx,
		"UPDATE cluster_meta SET bot_node_ip = ? WHERE id = 1", val,
	); err != nil {
		return fmt.Errorf("set bot_node_ip: %w", err)
	}
	return nil
}

// GetLanguage reads cluster_meta.language. Empty string means "never set" —
// callers fall back to i18n.Default. Synced cluster-wide so the interface
// language survives a bot-role transfer (V4-DESIGN §5.3): whichever node ends
// up talking to Telegram speaks the language the operator chose.
func GetLanguage(ctx context.Context, q Querier) (string, error) {
	var lang sql.NullString
	err := q.QueryRowContext(ctx, "SELECT language FROM cluster_meta WHERE id = 1").Scan(&lang)
	if err != nil {
		return "", fmt.Errorf("read language: %w", err)
	}
	return lang.String, nil
}

// SetLanguage sets (or, with lang=="", clears) the interface language.
// Runs inside the batch tx so the version bump and snapshot export capture it.
func SetLanguage(ctx context.Context, tx *sql.Tx, lang string) error {
	var val interface{}
	if lang != "" {
		val = lang
	} // else nil → SQL NULL (back to default)
	if _, err := tx.ExecContext(ctx,
		"UPDATE cluster_meta SET language = ? WHERE id = 1", val,
	); err != nil {
		return fmt.Errorf("set language: %w", err)
	}
	return nil
}

// Querier is the common subset of *sql.DB and *sql.Tx that read helpers use.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}
