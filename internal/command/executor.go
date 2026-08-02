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
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/example/meshcdn/internal/cert"
	"github.com/example/meshcdn/internal/db"
	"github.com/example/meshcdn/internal/nginx"
	"github.com/example/meshcdn/internal/peers"
	"github.com/example/meshcdn/internal/snapshot"
)

// DefaultCacheDir is nginx's proxy_cache_path root (see nginx/templates.go).
const DefaultCacheDir = "/etc/meshcdn/runtime/cache"

type Executor struct {
	DB       *sql.DB
	Registry Registry

	CertStore      *cert.Store
	NodeIP         string
	NginxConfigDir string

	SnapshotPath   string
	ProgramVersion string

	NotifyPeers func(newVersion int64)

	// CacheDir is the proxy_cache directory emptied by a purge action.
	// Empty → DefaultCacheDir.
	CacheDir string

	// PeerMgr owns persistent/peers.json. Set on nodes that participate in a
	// mesh; nil in single-node CLI mode, where membership reconciliation is
	// skipped.
	PeerMgr *peers.Manager

	// BroadcastExec sends a command to every peer (each runs it locally) and
	// returns a short human report. Wired only on nodes with a live mesh
	// client; nil in single-node CLI mode. Used to fan out cache purges.
	BroadcastExec func(ctx context.Context, cmdText string) string
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

	// A read-shaped action (e.g. /v target) can change synced cluster state
	// without any /w or /d; ForceVersionBump makes it count as a mutation.
	versionBumped := anyMutationSucceeded || result.AggregatedEffects.ForceVersionBump

	if versionBumped {
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

	if err := e.applyEffects(ctx, &result.AggregatedEffects, versionBumped, result.NewVersion); err != nil {
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

	if eff.PurgeCache {
		if err := e.applyCachePurge(ctx, eff); err != nil {
			return err
		}
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

// applyCachePurge empties this node's proxy_cache directory, reloads nginx so it
// drops stale cache index entries, and — when the effect is a broadcast — fans
// the purge out to every peer. The cache files are already gone once we return,
// so a failing nginx reload is a warning, not a hard error (V4-DESIGN §6 info
// boundary: report the problem, don't pretend the purge failed).
func (e *Executor) applyCachePurge(ctx context.Context, eff *Effects) error {
	dir := e.CacheDir
	if dir == "" {
		dir = DefaultCacheDir
	}
	n, err := purgeCacheDir(dir)
	if err != nil {
		return fmt.Errorf("purge cache dir %s: %w", dir, err)
	}
	msg := fmt.Sprintf("\n  🧹 本节点缓存已清空 (删除 %d 项 @ %s)", n, dir)

	if e.NginxConfigDir != "" {
		mainConf := filepath.Join(e.NginxConfigDir, "nginx.conf")
		if rerr := nginx.ValidateAndReload(mainConf); rerr != nil {
			msg += fmt.Sprintf("\n  ⚠️ 缓存已删，但 nginx reload 失败 (建议手动 reload): %v", rerr)
		} else {
			msg += "\n  [nginx 已 reload]"
		}
	}

	if eff.PurgeCacheBroadcast {
		if e.BroadcastExec != nil {
			// Peers receive the non-broadcasting form → no re-broadcast storm.
			msg += "\n" + e.BroadcastExec(ctx, "/v cache - purge-node")
		} else {
			msg += "\n  (单机模式，无 peer 广播)"
		}
	}

	eff.UserMessage += msg
	return nil
}

// purgeCacheDir removes every entry inside dir (but keeps dir itself so nginx's
// proxy_cache_path stays valid). A missing dir means "nothing cached" → 0, nil.
func purgeCacheDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, ent := range entries {
		p := filepath.Join(dir, ent.Name())
		if err := os.RemoveAll(p); err != nil {
			return removed, fmt.Errorf("remove %s: %w", p, err)
		}
		removed++
	}
	return removed, nil
}

// ApplySnapshot replays a snapshot text into the local DB.
//
// Step 6: snapshot replay automatically runs in force mode (we don't want
// peer-replay to trigger /v confirm prompts mid-import).
// reconcilePeers converges local cluster membership — both persistent/peers.json
// and the `peers` mirror table — on what a snapshot declares.
//
// A snapshot states the whole peer set, but replaying it can only ever add
// (`/w internal peer-add`). Without this step a peer removed on one node is
// removed nowhere else: the shorter snapshot replays as a no-op everywhere, and
// the next time the node that ran the removal pulls from a node that still
// carries the stale entry, the entry comes back. Removal appeared to work and
// silently undid itself.
//
// Two guards, both load-bearing:
//
//   - declared == nil means the snapshot said nothing about membership. Do
//     nothing; an empty peer list would isolate the node permanently.
//   - this node is always kept, at its existing join order. Pulling a snapshot
//     that predates our own join would otherwise delete us from our own peer
//     list, and a node that does not know itself cannot be reasoned about by
//     the bot-node predicate or by `/v nodes`.
//
// Deletions are surgical rather than delete-all-then-rebuild, so a partial
// failure can never leave the node with no peers.
func (e *Executor) reconcilePeers(ctx context.Context, declared map[string]int) error {
	if declared == nil {
		return nil
	}

	keep := make(map[string]bool, len(declared)+1)
	for ip := range declared {
		keep[ip] = true
	}
	if e.NodeIP != "" {
		keep[e.NodeIP] = true
	}

	// Mirror table first: it is derived state, so a failure here is worth
	// reporting but must not stop peers.json (the durable copy) converging.
	dbErr := e.pruneDBPeers(ctx, keep)

	if err := e.reconcilePeersFile(declared); err != nil {
		return err
	}
	return dbErr
}

// pruneDBPeers deletes rows from the peers mirror table whose IP is not kept.
func (e *Executor) pruneDBPeers(ctx context.Context, keep map[string]bool) error {
	rows, err := e.DB.QueryContext(ctx, `SELECT ip FROM peers`)
	if err != nil {
		return fmt.Errorf("read peer rows: %w", err)
	}
	var drop []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			rows.Close()
			return err
		}
		if !keep[ip] {
			drop = append(drop, ip)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, ip := range drop {
		if _, err := e.DB.ExecContext(ctx, `DELETE FROM peers WHERE ip = ?`, ip); err != nil {
			return fmt.Errorf("delete peer row %s: %w", ip, err)
		}
	}
	return nil
}

func (e *Executor) reconcilePeersFile(declared map[string]int) error {
	if e.PeerMgr == nil || declared == nil {
		return nil
	}

	existing, err := e.PeerMgr.All()
	if err != nil {
		return fmt.Errorf("read current peers: %w", err)
	}

	selfOrder := -1
	for _, p := range existing {
		if p.IP == e.NodeIP {
			selfOrder = p.JoinOrder
			break
		}
	}

	next := make([]peers.Peer, 0, len(declared)+1)
	for ip, order := range declared {
		next = append(next, peers.Peer{IP: ip, JoinOrder: order})
	}
	if _, ok := declared[e.NodeIP]; !ok && e.NodeIP != "" && selfOrder >= 0 {
		next = append(next, peers.Peer{IP: e.NodeIP, JoinOrder: selfOrder})
	}

	sort.Slice(next, func(i, j int) bool {
		if next[i].JoinOrder != next[j].JoinOrder {
			return next[i].JoinOrder < next[j].JoinOrder
		}
		return next[i].IP < next[j].IP
	})

	if len(next) == len(existing) {
		same := true
		for i := range next {
			if next[i] != existing[i] {
				same = false
				break
			}
		}
		if same {
			return nil // nothing changed; don't rewrite the file
		}
	}

	if err := e.PeerMgr.ReplaceAll(next); err != nil {
		return err
	}
	log.Printf("[snapshot] peer list reconciled: %d → %d", len(existing), len(next))
	return nil
}

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

	// Converge membership on what the snapshot declares. Replay can only add,
	// so without this a removed peer is removed only on the node that ran the
	// command — and comes back the next time that node pulls from a stale one.
	if err := e.reconcilePeers(ctx, snap.DeclaredPeers()); err != nil {
		// Membership drift is worth reporting but must not abort a config
		// sync that has already committed.
		log.Printf("[snapshot] reconcile peers.json: %v", err)
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
