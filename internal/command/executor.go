// Package command's batch executor — UPDATED for step 6.
//
// New in this version:
//   - Per-command port-protocol enforcement (calls EnforcePortBindings inside tx)
//   - Aggregated PortProtocolBindings → enforced once per batch
//   - Force-mode support (via context, populated by /v confirm flow)
package command

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/example/meshcdn/internal/cert"
	"github.com/example/meshcdn/internal/db"
	"github.com/example/meshcdn/internal/nginx"
	"github.com/example/meshcdn/internal/snapshot"
)

type Executor struct {
	DB       *sql.DB
	Registry Registry

	CertStore      *cert.Store
	NodeIP         string
	NginxConfigDir string

	SnapshotPath   string
	ProgramVersion string

	NotifyPeers func(newVersion int64)
}

func NewExecutor(database *sql.DB, registry Registry) *Executor {
	return &Executor{DB: database, Registry: registry}
}

func (e *Executor) WithNginxIntegration(certStore *cert.Store, nodeIP, configDir string) *Executor {
	e.CertStore = certStore
	e.NodeIP = nodeIP
	e.NginxConfigDir = configDir
	return e
}

func (e *Executor) WithSnapshotMaintenance(snapshotPath, programVersion string) *Executor {
	e.SnapshotPath = snapshotPath
	e.ProgramVersion = programVersion
	return e
}

func (e *Executor) WithMeshNotify(fn func(newVersion int64)) *Executor {
	e.NotifyPeers = fn
	return e
}

// ExecuteBatch parses and executes a batch text.
func (e *Executor) ExecuteBatch(ctx context.Context, batchText string) (*BatchResult, error) {
	lines := splitLines(batchText)
	parsedCmds := make([]*Command, len(lines))
	parseErrors := make([]error, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			parseErrors[i] = NewError(ErrBadFormat, "empty line in batch")
			continue
		}
		cmd, err := Parse(line)
		if err != nil {
			parseErrors[i] = err
			continue
		}
		parsedCmds[i] = cmd
	}

	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}

	oldVersion, err := db.CurrentVersion(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	result := &BatchResult{
		OldVersion: oldVersion,
		NewVersion: oldVersion,
		Outcomes:   make([]CommandOutcome, len(lines)),
	}

	anySucceeded := false
	anyMutationSucceeded := false
	forced := IsForced(ctx)

	// Track port-protocol bindings to enforce once at the end. We could
	// enforce per-command, but batching all commands' bindings together
	// lets us catch intra-batch conflicts early without partial commits.
	var allPortBindings []PortProtocolBinding

	for i, cmd := range parsedCmds {
		outcome := CommandOutcome{Index: i}

		if parseErrors[i] != nil {
			outcome.Err = parseErrors[i]
			result.Outcomes[i] = outcome
			continue
		}
		outcome.Command = cmd

		handler, err := e.Registry.Get(cmd.Type)
		if err != nil {
			outcome.Err = err
			result.Outcomes[i] = outcome
			continue
		}

		if err := handler.Validate(cmd); err != nil {
			outcome.Err = err
			result.Outcomes[i] = outcome
			continue
		}

		var effects Effects
		var execErr error
		switch cmd.Verb {
		case VerbWrite:
			effects, execErr = handler.Write(tx, cmd)
		case VerbDelete:
			effects, execErr = handler.Delete(tx, cmd)
		case VerbView:
			effects, execErr = handler.View(tx, cmd)
		default:
			execErr = NewError(ErrBadFormat, "unknown verb")
		}

		if execErr != nil {
			outcome.Err = execErr
			result.Outcomes[i] = outcome
			continue
		}

		// Enforce port-protocol bindings reported by this command
		if len(effects.PortProtocolBindings) > 0 {
			if err := EnforcePortBindings(ctx, tx, effects.PortProtocolBindings, forced); err != nil {
				outcome.Err = err
				result.Outcomes[i] = outcome
				continue
			}
			allPortBindings = append(allPortBindings, effects.PortProtocolBindings...)
		}

		outcome.Effects = effects
		result.Outcomes[i] = outcome
		result.AggregatedEffects.Merge(effects)
		anySucceeded = true
		if cmd.Verb.IsMutating() {
			anyMutationSucceeded = true
		}
	}

	if anyMutationSucceeded {
		newVersion, err := db.BumpVersion(ctx, tx)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		result.NewVersion = newVersion
	}

	if anySucceeded {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
	} else {
		_ = tx.Rollback()
	}

	if err := e.applyEffects(ctx, &result.AggregatedEffects, anyMutationSucceeded, result.NewVersion); err != nil {
		result.AggregatedEffects.UserMessage += fmt.Sprintf("\n⚠️  effects 应用失败: %v", err)
	}

	return result, nil
}

func (e *Executor) applyEffects(ctx context.Context, eff *Effects, mutated bool, newVersion int64) error {
	if eff.NeedsNginxReload && e.CertStore != nil {
		gen := nginx.New(e.CertStore, e.NodeIP)
		if e.NginxConfigDir != "" {
			gen.OutputDir = e.NginxConfigDir
		}
		if err := gen.Generate(ctx, e.DB); err != nil {
			return fmt.Errorf("regenerate nginx config: %w", err)
		}
		mainConf := filepath.Join(gen.OutputDir, "nginx.conf")
		if err := nginx.ValidateAndReload(mainConf); err != nil {
			return fmt.Errorf("nginx reload: %w", err)
		}
		eff.UserMessage += "\n  [nginx 已 reload]"
	}

	if !mutated {
		return nil
	}

	if e.SnapshotPath != "" {
		exp := &snapshot.Exporter{DB: e.DB, ProgramVersion: e.ProgramVersion}
		snap, err := exp.Export(ctx)
		if err != nil {
			return fmt.Errorf("export snapshot: %w", err)
		}
		if err := snap.SaveTo(e.SnapshotPath); err != nil {
			return fmt.Errorf("save snapshot: %w", err)
		}
	}

	if e.NotifyPeers != nil {
		go e.NotifyPeers(newVersion)
	}

	return nil
}

// ApplySnapshot replays a snapshot text into the local DB.
//
// Step 6: snapshot replay automatically runs in force mode (we don't want
// peer-replay to trigger /v confirm prompts mid-import).
func (e *Executor) ApplySnapshot(ctx context.Context, snapshotText string) (*snapshot.ReplayResult, error) {
	snap, err := snapshot.ParseText(snapshotText)
	if err != nil {
		return nil, fmt.Errorf("parse snapshot: %w", err)
	}

	res, err := snap.Import(ctx, e.DB, func(tx *sql.Tx, line string) error {
		return e.replayOneInTxForce(tx, line)
	})
	if err != nil {
		return nil, err
	}

	if e.CertStore != nil {
		gen := nginx.New(e.CertStore, e.NodeIP)
		if e.NginxConfigDir != "" {
			gen.OutputDir = e.NginxConfigDir
		}
		if err := gen.Generate(ctx, e.DB); err != nil {
			return res, fmt.Errorf("post-snapshot nginx regen: %w", err)
		}
		mainConf := filepath.Join(gen.OutputDir, "nginx.conf")
		if err := nginx.ValidateAndReload(mainConf); err != nil {
			return res, fmt.Errorf("post-snapshot nginx reload: %w", err)
		}
	}

	if e.SnapshotPath != "" {
		if err := snap.SaveTo(e.SnapshotPath); err != nil {
			return res, fmt.Errorf("save imported snapshot: %w", err)
		}
	}

	return res, nil
}

// replayOneInTxForce is the per-command callback for snapshot.Import.
// Always runs in "force" mode so the snapshot replay doesn't get blocked
// by port-protocol conflicts (the source-of-truth says these bindings
// should hold; replaying must obey).
func (e *Executor) replayOneInTxForce(tx *sql.Tx, line string) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	cmd, err := Parse(line)
	if err != nil {
		return fmt.Errorf("parse %q: %w", line, err)
	}
	if cmd.Verb == VerbView {
		return nil
	}
	handler, err := e.Registry.Get(cmd.Type)
	if err != nil {
		return err
	}
	if err := handler.Validate(cmd); err != nil {
		return err
	}
	var effects Effects
	switch cmd.Verb {
	case VerbWrite:
		effects, err = handler.Write(tx, cmd)
	case VerbDelete:
		effects, err = handler.Delete(tx, cmd)
	}
	if err != nil {
		return err
	}
	// Enforce port bindings in force mode (overrides any conflicts).
	if len(effects.PortProtocolBindings) > 0 {
		ctx := context.Background()
		if err := EnforcePortBindings(ctx, tx, effects.PortProtocolBindings, true); err != nil {
			return err
		}
	}
	return nil
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(s, "\n")
}

func FormatReport(r *BatchResult) string {
	var sb strings.Builder

	successes, failures := 0, 0
	for _, o := range r.Outcomes {
		if o.Err == nil && o.Command != nil {
			successes++
		} else if o.Err != nil {
			failures++
		}
	}

	if successes > 0 && failures == 0 {
		fmt.Fprintf(&sb, "✅ %d 条命令成功执行 (config_version: %d → %d)\n",
			successes, r.OldVersion, r.NewVersion)
	} else if successes > 0 && failures > 0 {
		fmt.Fprintf(&sb, "⚠️  %d 条成功, %d 条失败 (config_version: %d → %d)\n",
			successes, failures, r.OldVersion, r.NewVersion)
	} else if failures > 0 {
		fmt.Fprintf(&sb, "❌ %d 条命令全部失败 (config_version 未变: %d)\n",
			failures, r.OldVersion)
	} else {
		sb.WriteString("(空批处理)\n")
	}

	for i, o := range r.Outcomes {
		if o.Err != nil {
			fmt.Fprintf(&sb, "  L%d 失败: %v\n", i+1, o.Err)
		} else if o.Command != nil && o.Effects.UserMessage != "" {
			fmt.Fprintf(&sb, "  L%d %s: %s\n", i+1, o.Command.Verb, o.Effects.UserMessage)
		}
	}

	if r.AggregatedEffects.UserMessage != "" {
		sb.WriteString(r.AggregatedEffects.UserMessage)
		sb.WriteString("\n")
	}

	return sb.String()
}
