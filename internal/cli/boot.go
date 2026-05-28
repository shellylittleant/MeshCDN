// Boot-time DB rebuild from snapshot.cmd.
//
// Per V4-DESIGN §4.3 (升级 boot 流程):
//  1. Read 4 persistent files
//  2. Restore config_version from snapshot header
//  3. Rebuild database schema
//  4. Replay snapshot commands into db
//  5. Generate nginx config + start OpenResty (handled by systemd)
//  6. Join heartbeat network (handled in Serve)
//
// This file handles steps 2-4. Called from Serve() before starting any
// long-running goroutines.
//
// Safety:
//   - Only runs if config_version is 0 (fresh DB) AND snapshot.cmd exists.
//     If db has any state, we leave it alone — operator's responsibility
//     to delete the DB if they want a hard reset.
//   - Failures during replay are logged but don't abort agent startup.
//     The cluster will reconcile via mesh pull within a heartbeat cycle.
package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/example/meshcdn/internal/command"
	"github.com/example/meshcdn/internal/db"
	"github.com/example/meshcdn/internal/snapshot"
)

// MaybeRebuildFromSnapshot checks whether the DB is empty and snapshot.cmd
// exists; if both are true, it rebuilds the DB from the snapshot.
//
// Returns:
//   - true if rebuild happened (caller may want to log/notify)
//   - false if no rebuild was needed (db was already populated)
//   - error if rebuild was needed but failed
func MaybeRebuildFromSnapshot(ctx context.Context, conn *sql.DB, executor *command.Executor) (bool, error) {
	v, err := db.CurrentVersion(ctx, conn)
	if err != nil {
		return false, fmt.Errorf("read config_version: %w", err)
	}
	if v > 0 {
		// DB has state, leave alone
		return false, nil
	}

	snap, err := snapshot.Load()
	if errors.Is(err, snapshot.ErrNoSnapshot) {
		// Fresh install with no prior state, no snapshot to import — totally fine
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load snapshot: %w", err)
	}

	if len(snap.Commands) == 0 {
		// Empty snapshot — header only. Nothing to do.
		return false, nil
	}

	log.Printf("[boot] rebuilding from snapshot.cmd: %d commands, target version=%d",
		len(snap.Commands), snap.Header.Version)

	// Use ApplySnapshot to do the import — same code path as mesh pull.
	res, err := executor.ApplySnapshot(ctx, snap.String())
	if err != nil {
		return false, fmt.Errorf("apply snapshot: %w", err)
	}

	log.Printf("[boot] snapshot replay: %d applied, %d failed, version=%d",
		res.CommandsApplied, res.CommandsFailed, res.NewVersion)
	for idx, e := range res.PerLineErrors {
		log.Printf("[boot]   line %d failed: %v", idx, e)
	}

	return true, nil
}
