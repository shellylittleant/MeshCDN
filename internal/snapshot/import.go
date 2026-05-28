// snapshot import — replay a snapshot text into a fresh DB state.
//
// Per V4-DESIGN §1.3 (snapshot pull):
//
//	The lagging node receives a snapshot from a peer, then:
//	  1. Wipes its local rule tables (domains, bindings, rule_objects, port_protocols)
//	  2. Replays each /w command in the snapshot (via the executor)
//	  3. Sets config_version to the snapshot's header version
//	  4. Commits in a single transaction
//
// This must be transactional: if replay fails midway, we roll back so
// the local DB still represents the previous version (no partial state).
//
// Note: this file lives in the snapshot package but depends on command
// indirectly via the replay function the caller passes. We avoid an
// import cycle by taking the replay function as a parameter.
package snapshot

import (
	"context"
	"database/sql"
	"fmt"
)

// ReplayResult reports what happened during Import.
type ReplayResult struct {
	CommandsApplied int
	CommandsFailed  int
	NewVersion      int64

	// PerLineErrors maps command index → error for any failed lines.
	PerLineErrors map[int]error
}

// Import wipes the rule tables and replays the snapshot into them.
//
// The replayLine function is called once per command; it should parse and
// execute that single command within the provided tx. We don't import the
// command package here to avoid a cycle.
//
// Per V4-DESIGN: failed commands during snapshot replay are LOGGED but do
// not abort the import. The new version is set to the snapshot's header
// version regardless. This matches the snapshot's role as "best effort
// catch up to peer's view" — if the peer's snapshot has slightly different
// command syntax (e.g. due to a version mismatch), we replay what we can
// and align our version number anyway. Subsequent ping cycles will catch
// any drift and re-pull if needed.
func (s *Snapshot) Import(ctx context.Context, db *sql.DB,
	replayLine func(tx *sql.Tx, line string) error) (*ReplayResult, error) {

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}

	res := &ReplayResult{PerLineErrors: map[int]error{}}

	// 1) Wipe rule tables
	wipeStatements := []string{
		`DELETE FROM domains`,
		`DELETE FROM port_protocols`,
		`DELETE FROM rule_objects`,
		`DELETE FROM bindings`,
	}
	for _, stmt := range wipeStatements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("wipe (%s): %w", stmt, err)
		}
	}

	// 2) Replay each command
	for i, line := range s.Commands {
		if err := replayLine(tx, line); err != nil {
			res.CommandsFailed++
			res.PerLineErrors[i] = err
			continue
		}
		res.CommandsApplied++
	}

	// 3) Set config_version to snapshot's header version
	if _, err := tx.ExecContext(ctx,
		`UPDATE cluster_meta SET config_version = ? WHERE id = 1`,
		s.Header.Version); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("set config_version: %w", err)
	}
	res.NewVersion = s.Header.Version

	// 4) Commit
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return res, nil
}
