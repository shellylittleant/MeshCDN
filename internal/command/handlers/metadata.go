// Cluster metadata view handlers — /v export, /v status, /v nodes, /v stats.
//
// Per V4-DESIGN §8.3 (集群元数据查询):
//
//	These types only support /v (read-only). /w and /d on them return
//	ErrBadFormat. Some accept scope (e.g. /v nodes 1.2.3.4 -); others require
//	scope=- (e.g. /v export - -).
//
// `stats` is currently a stub — we'll fill it in once we have real traffic
// stats from logs.db (step 7+).
package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/example/meshcdn/internal/command"
	"github.com/example/meshcdn/internal/db"
	"github.com/example/meshcdn/internal/i18n"
	"github.com/example/meshcdn/internal/logs"
	"github.com/example/meshcdn/internal/peers"
	"github.com/example/meshcdn/internal/snapshot"
)

// ─────────────────────────────────────────────────────────────────────
// /v export — output the snapshot.cmd-format text
// ─────────────────────────────────────────────────────────────────────

type ExportHandler struct {
	DB             *sql.DB
	ProgramVersion string
}

func (h *ExportHandler) Type() string { return "export" }

func (h *ExportHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "export", nil
}

func (h *ExportHandler) Validate(cmd *command.Command) error {
	if cmd.Verb != command.VerbView {
		return command.NewError(command.ErrBadFormat,
			"/v export only — export is read-only")
	}
	if !command.IsPlaceholder(cmd.Scope) {
		return command.NewError(command.ErrBadFormat,
			"/v export must use - for scope")
	}
	return nil
}

func (h *ExportHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "export is read-only")
}

func (h *ExportHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "export is read-only")
}

func (h *ExportHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	exp := &snapshot.Exporter{DB: h.DB, ProgramVersion: h.ProgramVersion}
	snap, err := exp.Export(context.Background())
	if err != nil {
		return command.Effects{}, fmt.Errorf("export: %w", err)
	}

	body := snap.String()

	// V4.0.19: return as file attachment rather than chat message. The whole
	// point of an export is to be re-imported, so a downloadable .txt is
	// strictly more useful than chat-paginated text — and there's no
	// 4096-character ceiling.
	//
	// Filename embeds config_version + UTC timestamp for sortability and
	// audit (matches the file you'll see in Telegram group history).
	ver, _ := db.CurrentVersion(context.Background(), tx)
	stamp := time.Now().UTC().Format("20060102T150405Z")
	filename := fmt.Sprintf("meshcdn-export-v%d-%s.txt", ver, stamp)

	// Quick stats for the accompanying caption. Counts /w lines as commands;
	// the body itself may have a header comment block.
	cmdCount := 0
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "/w ") {
			cmdCount++
		}
	}

	return command.Effects{
		UserMessage: cmd.T("export.caption", ver, cmdCount),
		FileAttachment: &command.FileAttachment{
			Filename: filename,
			Content:  []byte(body),
			MIMEHint: "text/plain; charset=utf-8",
		},
	}, nil
}

// ─────────────────────────────────────────────────────────────────────
// /v status — local node status
// ─────────────────────────────────────────────────────────────────────

type StatusHandler struct {
	NodeIP         string
	ProgramVersion string
	PeerMgr        *peers.Manager
}

func (h *StatusHandler) Type() string { return "status" }

func (h *StatusHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "status", nil
}

func (h *StatusHandler) Validate(cmd *command.Command) error {
	if cmd.Verb != command.VerbView {
		return command.NewError(command.ErrBadFormat, "/v status only")
	}
	if !command.IsPlaceholder(cmd.Scope) {
		return command.NewError(command.ErrBadFormat, "/v status must use - for scope")
	}
	return nil
}

func (h *StatusHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "status is read-only")
}
func (h *StatusHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "status is read-only")
}

func (h *StatusHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	ctx := context.Background()

	v, err := db.CurrentVersion(ctx, tx)
	if err != nil {
		return command.Effects{}, err
	}

	var domainCount, ruleObjCount, bindingCount int
	_ = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM domains").Scan(&domainCount)
	_ = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM rule_objects").Scan(&ruleObjCount)
	_ = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM bindings").Scan(&bindingCount)

	peerCount := 0
	if h.PeerMgr != nil {
		all, _ := h.PeerMgr.All()
		peerCount = len(all)
	}

	botIP := effectiveBotIP(ctx, tx, h.PeerMgr)
	botLabel := botIP
	if botIP == "" {
		botLabel = cmd.T("status.unknown")
	} else if botIP == h.NodeIP {
		botLabel = botIP + " " + cmd.T("status.this_node")
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", cmd.T("status.title"))
	fmt.Fprintf(&sb, "  %s %s\n", i18n.PadRight(cmd.T("status.node_ip")+":", 20), h.NodeIP)
	fmt.Fprintf(&sb, "  %s %s\n", i18n.PadRight(cmd.T("status.bot_node")+":", 20), botLabel)
	fmt.Fprintf(&sb, "  %s %s\n", i18n.PadRight(cmd.T("status.program")+":", 20), h.ProgramVersion)
	fmt.Fprintf(&sb, "  %s %d\n", i18n.PadRight(cmd.T("status.config_ver")+":", 20), v)
	fmt.Fprintf(&sb, "  %s %d\n", i18n.PadRight(cmd.T("status.peer_count")+":", 20), peerCount)
	fmt.Fprintf(&sb, "  %s %d\n", i18n.PadRight(cmd.T("status.domains")+":", 20), domainCount)
	fmt.Fprintf(&sb, "  %s %d\n", i18n.PadRight(cmd.T("status.rule_objs")+":", 20), ruleObjCount)
	fmt.Fprintf(&sb, "  %s %d\n", i18n.PadRight(cmd.T("status.bindings")+":", 20), bindingCount)
	fmt.Fprintf(&sb, "  %s %s\n", i18n.PadRight(cmd.T("status.language")+":", 20), cmd.Lang.DisplayName())
	fmt.Fprintf(&sb, "  %s %s\n", i18n.PadRight(cmd.T("status.now")+":", 20), time.Now().UTC().Format(time.RFC3339))

	return command.Effects{UserMessage: sb.String()}, nil
}

// effectiveBotIP resolves which node currently owns the bot role: the explicit
// cluster_meta.bot_node_ip override if set, otherwise the join-order default
// (lowest join_order, via peers.BotNodeIP). This is the single source of truth
// shared by status/nodes display, alert routing, and the boot gate.
func effectiveBotIP(ctx context.Context, q db.Querier, pm *peers.Manager) string {
	if override, err := db.GetBotNodeIP(ctx, q); err == nil && override != "" {
		return override
	}
	if pm != nil {
		return pm.BotNodeIP()
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────
// /v nodes — peer list (or single peer details)
// ─────────────────────────────────────────────────────────────────────

type NodesHandler struct {
	PeerMgr *peers.Manager

	PeerStateProvider func() map[string]PeerHeartbeatState
}

type PeerHeartbeatState struct {
	LastSeenAt          time.Time
	LastRTT             time.Duration
	ConfigVersion       int64
	ProgramVersion      string
	ConsecutiveFailures int
}

func (h *NodesHandler) Type() string { return "nodes" }

func (h *NodesHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "nodes:" + scope, nil
}

func (h *NodesHandler) Validate(cmd *command.Command) error {
	if cmd.Verb != command.VerbView {
		return command.NewError(command.ErrBadFormat, "/v nodes only")
	}
	return nil
}

func (h *NodesHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "nodes is read-only")
}
func (h *NodesHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "nodes is read-only")
}

func (h *NodesHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	if h.PeerMgr == nil {
		return command.Effects{}, command.NewError(command.ErrInternal, "peer manager not configured")
	}
	all, err := h.PeerMgr.All()
	if err != nil {
		return command.Effects{}, err
	}

	var states map[string]PeerHeartbeatState
	if h.PeerStateProvider != nil {
		states = h.PeerStateProvider()
	}

	var sb strings.Builder

	if !command.IsPlaceholder(cmd.Scope) {
		for _, p := range all {
			if p.IP == cmd.Scope {
				fmt.Fprintf(&sb, "Peer %s\n", p.IP)
				fmt.Fprintf(&sb, "  join_order: %d\n", p.JoinOrder)
				if st, ok := states[p.IP]; ok {
					fmt.Fprintf(&sb, "  last_seen:  %s\n", st.LastSeenAt.Format(time.RFC3339))
					fmt.Fprintf(&sb, "  rtt:        %s\n", st.LastRTT)
					fmt.Fprintf(&sb, "  version:    %d\n", st.ConfigVersion)
					fmt.Fprintf(&sb, "  failures:   %d\n", st.ConsecutiveFailures)
				} else {
					sb.WriteString("  (no heartbeat data — local node or mesh disabled)\n")
				}
				return command.Effects{UserMessage: sb.String()}, nil
			}
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("peer %s not found", cmd.Scope),
		}, nil
	}

	botIP := effectiveBotIP(context.Background(), tx, h.PeerMgr)
	fmt.Fprintf(&sb, "%s\n", cmd.T("nodes.title", len(all)))
	for _, p := range all {
		mark := "  "
		if p.IP == botIP {
			mark = "⭐"
		}
		st, ok := states[p.IP]
		if ok {
			rttStr := "—"
			if st.LastRTT > 0 {
				rttStr = st.LastRTT.String()
			}
			status := cmd.T("nodes.online")
			if st.ConsecutiveFailures >= 3 {
				status = cmd.T("nodes.offline")
			} else if st.ConsecutiveFailures > 0 {
				status = "degraded"
			}
			fmt.Fprintf(&sb, "%s [%d] %-15s  v%-4d  rtt=%-10s  %s\n",
				mark, p.JoinOrder, p.IP, st.ConfigVersion, rttStr, status)
		} else {
			fmt.Fprintf(&sb, "%s [%d] %-15s  %s\n", mark, p.JoinOrder, p.IP, cmd.T("nodes.no_data"))
		}
	}

	return command.Effects{UserMessage: sb.String()}, nil
}

// ─────────────────────────────────────────────────────────────────────
// /v stats — traffic stats from logs.db (this node only)
//
//	/v stats -        -         全部域名, 近 24h
//	/v stats a.com    -         单域名, 近 24h
//	/v stats a.com    7d        单域名, 近 7 天  (窗口: Nh / Nd / Go duration)
//
// Single-node scope (V4-DESIGN task 3 阶段 3.2): numbers reflect requests this
// node served. Cross-node aggregation (3.3) is a follow-up.
// ─────────────────────────────────────────────────────────────────────

type StatsHandler struct {
	// Store is this node's logs.db. nil when the collector isn't running
	// (e.g. mesh/nginx disabled), in which case /v stats reports "未启用".
	Store *logs.Store
}

func (h *StatsHandler) Type() string { return "stats" }

func (h *StatsHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "stats", nil
}

func (h *StatsHandler) Validate(cmd *command.Command) error {
	if cmd.Verb != command.VerbView {
		return command.NewError(command.ErrBadFormat, "/v stats only")
	}
	return nil
}

func (h *StatsHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "stats is read-only")
}
func (h *StatsHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "stats is read-only")
}
func (h *StatsHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	if h.Store == nil {
		return command.Effects{
			UserMessage: cmd.T("stats.disabled"),
		}, nil
	}
	ctx := context.Background()

	window := parseStatsWindow(cmd.Params)
	since := time.Now().Add(-window).Truncate(time.Minute).Unix()

	domain := ""
	if !command.IsPlaceholder(cmd.Scope) {
		domain = cmd.Scope
	}

	rows, err := h.Store.StatusBreakdown(ctx, domain, since)
	if err != nil {
		return command.Effects{}, fmt.Errorf("stats query: %w", err)
	}

	scopeLabel := domain
	if scopeLabel == "" {
		scopeLabel = cmd.T("stats.all_domains")
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", cmd.T("stats.title", scopeLabel, humanWindow(window)))

	var totalHits, totalBytes int64
	for _, r := range rows {
		totalHits += r.Hits
		totalBytes += r.Bytes
	}
	if totalHits == 0 {
		sb.WriteString(cmd.T("stats.no_data") + "\n")
		return command.Effects{UserMessage: sb.String()}, nil
	}

	fmt.Fprintf(&sb, "%s\n", cmd.T("stats.total_hits", totalHits))
	fmt.Fprintf(&sb, "%s\n", cmd.T("stats.total_bytes", humanBytes(totalBytes)))
	sb.WriteString(cmd.T("stats.by_status") + "\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "    %d: %d (%.1f%%)\n",
			r.Status, r.Hits, 100*float64(r.Hits)/float64(totalHits))
	}

	if domain == "" {
		tops, err := h.Store.TopDomains(ctx, since, 10)
		if err == nil && len(tops) > 0 {
			sb.WriteString(cmd.T("stats.top_domains") + "\n")
			for _, t := range tops {
				fmt.Fprintf(&sb, "    %-24s %d %s  %s\n", t.Domain, t.Hits, cmd.T("stats.hits_unit"), humanBytes(t.Bytes))
			}
		}
	}

	return command.Effects{UserMessage: sb.String()}, nil
}

// parseStatsWindow accepts "Nd" (days), "Nh"/Go durations, or "-"/"" (→ 24h).
func parseStatsWindow(params string) time.Duration {
	p := strings.TrimSpace(params)
	if p == "" || p == "-" {
		return 24 * time.Hour
	}
	if strings.HasSuffix(p, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(p, "d")); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	if d, err := time.ParseDuration(p); err == nil && d > 0 {
		return d
	}
	return 24 * time.Hour
}

func humanWindow(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
