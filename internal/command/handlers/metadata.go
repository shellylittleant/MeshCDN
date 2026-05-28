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
	"strings"
	"time"

	"github.com/example/meshcdn/internal/command"
	"github.com/example/meshcdn/internal/db"
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
		UserMessage: fmt.Sprintf("📤 配置 v%d — %d 条命令", ver, cmdCount),
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

	var sb strings.Builder
	fmt.Fprintf(&sb, "MeshCDN 节点状态\n")
	fmt.Fprintf(&sb, "  节点 IP:        %s\n", h.NodeIP)
	fmt.Fprintf(&sb, "  程序版本:       %s\n", h.ProgramVersion)
	fmt.Fprintf(&sb, "  config_version: %d\n", v)
	fmt.Fprintf(&sb, "  集群节点数:     %d\n", peerCount)
	fmt.Fprintf(&sb, "  域名规则:       %d\n", domainCount)
	fmt.Fprintf(&sb, "  规则对象:       %d\n", ruleObjCount)
	fmt.Fprintf(&sb, "  绑定关系:       %d\n", bindingCount)
	fmt.Fprintf(&sb, "  当前时间:       %s\n", time.Now().UTC().Format(time.RFC3339))

	return command.Effects{UserMessage: sb.String()}, nil
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

	fmt.Fprintf(&sb, "集群节点 (%d):\n", len(all))
	for _, p := range all {
		st, ok := states[p.IP]
		if ok {
			rttStr := "—"
			if st.LastRTT > 0 {
				rttStr = st.LastRTT.String()
			}
			status := "online"
			if st.ConsecutiveFailures >= 3 {
				status = "offline"
			} else if st.ConsecutiveFailures > 0 {
				status = "degraded"
			}
			fmt.Fprintf(&sb, "  [%d] %-15s  v%-4d  rtt=%-10s  %s\n",
				p.JoinOrder, p.IP, st.ConfigVersion, rttStr, status)
		} else {
			fmt.Fprintf(&sb, "  [%d] %-15s  (local or no data)\n", p.JoinOrder, p.IP)
		}
	}

	return command.Effects{UserMessage: sb.String()}, nil
}

// ─────────────────────────────────────────────────────────────────────
// /v stats — traffic stats (stub for step 5; real impl in step 7)
// ─────────────────────────────────────────────────────────────────────

type StatsHandler struct{}

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
	return command.Effects{
		UserMessage: "stats: 流量统计将在 step 7 实现 (依赖 logs.db 收集器)",
	}, nil
}
