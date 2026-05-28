// Port-protocol global enforcement.
//
// Per V4-DESIGN §8.7:
//
//	Each port has exactly one protocol cluster-wide. Trying to redefine
//	triggers ErrPortConflict, which the bot/cli surfaces as a /v confirm
//	prompt; user re-issues with a force flag to override.
//
// Step 6 implementation: after a /w domain command's handler returns
// PortProtocolBindings in Effects, the executor calls EnforcePortBindings
// inside the same transaction. Conflicts roll back the row write and
// return ErrPortConflict.
//
// Implementation lives in this file (separate from executor.go for clarity)
// because port-protocol is a cross-cutting concern, not specific to any
// one handler.
package command

import (
	"context"
	"database/sql"
	"fmt"
)

// EnforcePortBindings checks the proposed PortProtocolBindings against the
// current port_protocols table within tx. If there's a conflict (different
// protocol on a port that's already declared), returns ErrPortConflict.
//
// Otherwise, upserts the bindings into port_protocols.
//
// The "force" flag bypasses conflict detection (used by /v confirm flow).
func EnforcePortBindings(ctx context.Context, tx *sql.Tx,
	bindings []PortProtocolBinding, force bool) error {

	for _, b := range bindings {
		var existing string
		err := tx.QueryRowContext(ctx,
			`SELECT protocol FROM port_protocols WHERE port = ?`, b.Port,
		).Scan(&existing)

		switch {
		case err == sql.ErrNoRows:
			// New port — insert
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO port_protocols (port, protocol) VALUES (?, ?)`,
				b.Port, b.Protocol); err != nil {
				return fmt.Errorf("insert port_protocols: %w", err)
			}

		case err != nil:
			return fmt.Errorf("check port_protocols: %w", err)

		case existing != b.Protocol:
			// Conflict
			if !force {
				return NewError(ErrPortConflict,
					fmt.Sprintf("端口 %d 已绑定为 %s; 试图改为 %s",
						b.Port, existing, b.Protocol))
			}
			// Force: update
			if _, err := tx.ExecContext(ctx,
				`UPDATE port_protocols SET protocol = ? WHERE port = ?`,
				b.Protocol, b.Port); err != nil {
				return fmt.Errorf("update port_protocols: %w", err)
			}
		}
		// else: existing matches b.Protocol → no-op
	}
	return nil
}

// CheckPortBindingsForce reads the executor's optional "force" flag from
// command context. We use a tiny convention: if the BatchResult was
// constructed via the /v confirm resolver, it injects a force=true context
// value. Default is false (normal path).
//
// In step 6 the integration is simple: executor checks env var
// MESHCDN_FORCE for a single-shot override, OR the in-memory Pending
// Confirm flow re-issues the command via a re-entrant call with force=true.
//
// This minimal helper just reads from a context value; serve.go wires it.
type forceKey struct{}

// WithForce returns a child context flagged for force-mode execution.
func WithForce(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceKey{}, true)
}

// IsForced reports whether ctx is force-flagged.
func IsForced(ctx context.Context) bool {
	v, ok := ctx.Value(forceKey{}).(bool)
	return ok && v
}
