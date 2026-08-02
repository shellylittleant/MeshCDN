// Package logs owns runtime/logs.db — per-minute aggregated access statistics.
//
// Per V4-DESIGN §A.1 logs.db lives under runtime/ (wiped on upgrade, not synced
// across the cluster). We deliberately store *aggregates* keyed by
// (domain, minute, status), never raw per-request rows: raw logs would explode
// in size and don't belong in the rebuildable runtime tree.
//
// Two consumers share one *sql.DB: the collector (writes) and the StatsHandler
// (reads). SQLite in WAL mode handles that concurrency within one process.
package logs

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

const schema = `
-- Per-minute aggregated request counts. One row per (domain, minute, status).
CREATE TABLE IF NOT EXISTS request_stats (
    domain  TEXT    NOT NULL,
    minute  INTEGER NOT NULL,   -- unix epoch truncated to the minute
    status  INTEGER NOT NULL,   -- HTTP status code
    hits    INTEGER NOT NULL DEFAULT 0,
    bytes   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (domain, minute, status)
);
CREATE INDEX IF NOT EXISTS idx_request_stats_minute ON request_stats(minute);

-- Single-row collector bookkeeping: how far we've consumed the stats log.
CREATE TABLE IF NOT EXISTS collector_state (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    log_offset INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO collector_state (id, log_offset) VALUES (1, 0);
`

// Store wraps runtime/logs.db.
type Store struct {
	db *sql.DB
}

// StatKey identifies an aggregation bucket.
type StatKey struct {
	Domain string
	Minute int64
	Status int
}

// StatDelta is the increment for one bucket.
type StatDelta struct {
	Hits  int64
	Bytes int64
}

// StatusRow is one row of a status-code breakdown.
type StatusRow struct {
	Status int
	Hits   int64
	Bytes  int64
}

// DomainTotal is one row of a per-domain rollup.
type DomainTotal struct {
	Domain string
	Hits   int64
	Bytes  int64
}

// Open opens (creating if needed) logs.db at path and ensures the schema.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", path)
	conn, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open logs.db at %s: %w", path, err)
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping logs.db: %w", err)
	}
	if _, err := conn.ExecContext(context.Background(), schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("apply logs schema: %w", err)
	}
	return &Store{db: conn}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Offset returns the last consumed byte offset of the stats log.
func (s *Store) Offset(ctx context.Context) (int64, error) {
	var off int64
	err := s.db.QueryRowContext(ctx,
		"SELECT log_offset FROM collector_state WHERE id = 1").Scan(&off)
	if err != nil {
		return 0, fmt.Errorf("read offset: %w", err)
	}
	return off, nil
}

// Commit atomically applies a batch of bucket deltas and advances the stored
// log offset — so a crash mid-batch never double-counts on the next run.
func (s *Store) Commit(ctx context.Context, deltas map[StatKey]StatDelta, newOffset int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO request_stats (domain, minute, status, hits, bytes)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(domain, minute, status) DO UPDATE SET
		  hits  = hits  + excluded.hits,
		  bytes = bytes + excluded.bytes
	`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for k, d := range deltas {
		if _, err := stmt.ExecContext(ctx, k.Domain, k.Minute, k.Status, d.Hits, d.Bytes); err != nil {
			stmt.Close()
			_ = tx.Rollback()
			return err
		}
	}
	stmt.Close()
	if _, err := tx.ExecContext(ctx,
		"UPDATE collector_state SET log_offset = ? WHERE id = 1", newOffset); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ResetOffset sets the stored offset (used when the log file rotated/shrank).
func (s *Store) ResetOffset(ctx context.Context, off int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE collector_state SET log_offset = ? WHERE id = 1", off)
	return err
}

// Prune deletes aggregates older than the given minute bucket.
func (s *Store) Prune(ctx context.Context, beforeMinute int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM request_stats WHERE minute < ?", beforeMinute)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// StatusBreakdown returns hits/bytes grouped by status code for one domain
// (or all domains when domain==""), counting buckets at minute >= sinceMinute.
func (s *Store) StatusBreakdown(ctx context.Context, domain string, sinceMinute int64) ([]StatusRow, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if domain == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT status, SUM(hits), SUM(bytes) FROM request_stats
			WHERE minute >= ? GROUP BY status ORDER BY status`, sinceMinute)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT status, SUM(hits), SUM(bytes) FROM request_stats
			WHERE minute >= ? AND domain = ? GROUP BY status ORDER BY status`, sinceMinute, domain)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatusRow
	for rows.Next() {
		var r StatusRow
		if err := rows.Scan(&r.Status, &r.Hits, &r.Bytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TopDomains returns per-domain totals (highest hits first) since sinceMinute.
func (s *Store) TopDomains(ctx context.Context, sinceMinute int64, limit int) ([]DomainTotal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT domain, SUM(hits), SUM(bytes) FROM request_stats
		WHERE minute >= ? GROUP BY domain ORDER BY SUM(hits) DESC LIMIT ?`, sinceMinute, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DomainTotal
	for rows.Next() {
		var d DomainTotal
		if err := rows.Scan(&d.Domain, &d.Hits, &d.Bytes); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
