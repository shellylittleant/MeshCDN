// System action commands.
//
// Per V4-DESIGN §8.3:
//
//	These are independent verbs (not /w /d /v) but for parser uniformity
//	we still require strict 4-segment form: /sync - - -, /target <ip> - -.
//	The first segment determines which handler to invoke; remaining 3 are
//	handler-specific.
//
// To avoid forking the parser, we treat these as a special case:
//   - The action name (sync/target/upgrade/help/menu/confirm) is parsed
//     as cmd.Verb (a non-w/d/v verb).
//   - cmd.Type/Scope/Params follow as before.
//
// We extend the parser in step 5 to accept these system verbs.
package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"

	"github.com/example/meshcdn/internal/command"
	"github.com/example/meshcdn/internal/db"
	"github.com/example/meshcdn/internal/peers"
)

// SystemHandler is dispatched on system verbs; the verb itself goes into
// cmd.Type (so the registry maps "sync" → SystemHandler).
//
// In step 5, sync/target/upgrade/help/menu/confirm each get their own
// stub handler. They share the SystemHandler base because their
// validation rules are similar (all view-shaped).
type SyncHandler struct {
	NotifyAll func(ctx context.Context) error
}

func (h *SyncHandler) Type() string { return "sync" }
func (h *SyncHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "sync", nil
}
func (h *SyncHandler) Validate(cmd *command.Command) error {
	// /sync uses VerbView since it's a read-shaped action that doesn't write
	if cmd.Verb != command.VerbView {
		return command.NewError(command.ErrBadFormat, "/sync expected as /v sync - -")
	}
	return nil
}
func (h *SyncHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v sync - -")
}
func (h *SyncHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v sync - -")
}
func (h *SyncHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	if h.NotifyAll == nil {
		return command.Effects{}, command.NewError(command.ErrInternal, "sync not wired (mesh disabled?)")
	}
	if err := h.NotifyAll(context.Background()); err != nil {
		return command.Effects{}, fmt.Errorf("sync: %w", err)
	}
	return command.Effects{
		UserMessage: "已向所有 peer 推送 notify-version, peers 将在收到后主动拉同步",
	}, nil
}

// ─────────────────────────────────────────────────────────────────────
// /v target <peer-ip> -  — transfer bot role to specified peer
// ─────────────────────────────────────────────────────────────────────

type TargetHandler struct {
	// PeerMgr validates the target IP is a known cluster peer and provides the
	// join-order default bot node. Wired by serve.go / exec.go.
	PeerMgr *peers.Manager
	// LocalNodeIP is this node's IP, for friendlier messaging.
	LocalNodeIP string
}

func (h *TargetHandler) Type() string { return "target" }
func (h *TargetHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "target:" + scope, nil
}
func (h *TargetHandler) Validate(cmd *command.Command) error {
	if cmd.Verb != command.VerbView {
		return command.NewError(command.ErrBadFormat, "use /v target <peer-ip> -")
	}
	if command.IsPlaceholder(cmd.Scope) {
		return command.NewError(command.ErrBadFormat, "/v target requires <peer-ip> in scope")
	}
	if net.ParseIP(cmd.Scope) == nil {
		return command.NewError(command.ErrBadFormat,
			fmt.Sprintf("invalid peer IP: %q", cmd.Scope))
	}
	return nil
}
func (h *TargetHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v target")
}
func (h *TargetHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v target")
}

// View transfers the bot role to the given peer. It persists an explicit
// bot-node override in cluster_meta.bot_node_ip and forces a version bump so the
// change rides the config-version stream to every node (snapshot replay applies
// `/w internal set-bot <ip>`). Per the state-layer scope, the target actually
// begins polling Telegram — and the origin stops — on its next agent restart;
// the boot gate reads this override. Live (no-restart) switching is a separate
// follow-up.
func (h *TargetHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	ctx := context.Background()
	target := cmd.Scope

	// Target must be a known cluster peer.
	if h.PeerMgr != nil {
		all, err := h.PeerMgr.All()
		if err != nil {
			return command.Effects{}, fmt.Errorf("load peers: %w", err)
		}
		known := false
		for _, p := range all {
			if p.IP == target {
				known = true
				break
			}
		}
		if !known {
			return command.Effects{}, command.NewError(command.ErrNotFound,
				fmt.Sprintf("target %s 不是已知集群节点 (先 /v nodes - - 查看，或让它先认亲)", target))
		}
	}

	current, _ := db.GetBotNodeIP(ctx, tx)
	if current == "" && h.PeerMgr != nil {
		current = h.PeerMgr.BotNodeIP() // join-order default
	}
	if current == target {
		return command.Effects{
			UserMessage: fmt.Sprintf("%s 已经是当前 bot 节点，无需转移", target),
		}, nil
	}

	if err := db.SetBotNodeIP(ctx, tx, target); err != nil {
		return command.Effects{}, err
	}

	return command.Effects{
		ForceVersionBump: true,
		UserMessage: fmt.Sprintf(
			"✅ bot 角色已转移至 %s (全集群同步)。\n⚠️ 目标节点需在下次 agent 重启后才开始接管 Telegram 轮询；原 bot 节点重启后停止轮询。",
			target),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────
// /v upgrade - - - — trigger cluster-wide upgrade
// ─────────────────────────────────────────────────────────────────────

type UpgradeHandler struct {
	// OnUpgrade triggers the cluster upgrade and returns a per-node report
	// (which nodes confirmed the new version, which failed). Wired by serve.go.
	OnUpgrade func(ctx context.Context) (string, error)
}

func (h *UpgradeHandler) Type() string { return "upgrade" }
func (h *UpgradeHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "upgrade", nil
}
func (h *UpgradeHandler) Validate(cmd *command.Command) error {
	if cmd.Verb != command.VerbView {
		return command.NewError(command.ErrBadFormat, "use /v upgrade - -")
	}
	return nil
}
func (h *UpgradeHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v upgrade")
}
func (h *UpgradeHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v upgrade")
}
func (h *UpgradeHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	if h.OnUpgrade == nil {
		return command.Effects{
			UserMessage: "upgrade not wired (binary distribution not yet available)",
		}, nil
	}
	// OnUpgrade returns a per-node report even on partial failure — surfacing
	// which nodes upgraded is the whole point, so we show the report either way.
	report, err := h.OnUpgrade(context.Background())
	if report == "" && err != nil {
		return command.Effects{}, fmt.Errorf("upgrade: %w", err)
	}
	if report == "" {
		report = "已触发集群升级"
	}
	return command.Effects{UserMessage: report}, nil
}

// ─────────────────────────────────────────────────────────────────────
// /v help - - - — print command reference
// ─────────────────────────────────────────────────────────────────────

type HelpHandler struct{}

func (h *HelpHandler) Type() string { return "help" }
func (h *HelpHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "help", nil
}
func (h *HelpHandler) Validate(cmd *command.Command) error {
	if cmd.Verb != command.VerbView {
		return command.NewError(command.ErrBadFormat, "use /v help - -")
	}
	return nil
}
func (h *HelpHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v help")
}
func (h *HelpHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v help")
}
func (h *HelpHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{UserMessage: helpText}, nil
}

const helpText = `MeshCDN 命令参考

格式: /<verb> <type> <scope> <params>   (严格四段式，空位用 - 占位)

A 类 - 直接命令
  /w domain   <host:port>       <origin>             写入域名
  /w ssl      <域名/IP>          -                    申请 LE 证书
  /w sslfile  <域名/IP>          -                    上传证书 (env vars)
  /d <type>   <scope>           <params>             删除 (与 /w 镜像)

B 类 - 对象命令 (step 6)
  /w cache    <名字>            <key=value...>      定义缓存对象
  /v cache    -                 purge-all           清空全集群缓存并 reload
  /v cache    -                 purge-node          仅清空本节点缓存
  /w defense  <名字>            <key=value...>      定义防御对象
  /w redirect <名字>            <key=value...>      定义重定向对象
  /w header   <名字>            <key=value...>      定义头部对象

C 类 - 绑定命令 (step 6)
  /w bind     <域名/IP>          <对象类型>:<对象名>

V 类 - 查询命令
  /v <type>   <scope|->         <params|->          查询规则
  /v export   -                 -                    导出全集群配置
  /v status   -                 -                    本节点状态
  /v nodes    [<peer-ip>|-]     -                    peer 列表/详情
  /v stats    [<域名>|-]        [<窗口>|-]           流量统计 (本节点; 窗口如 24h/7d, 默认 24h)

系统动作 (read-shaped, 用 /v 调用)
  /v sync     -                 -                    强制同步给所有 peer
  /v target   <peer-ip>         -                    转移 bot 角色 (重启后生效)
  /v upgrade  -                 -                    触发集群升级
  /v help     -                 -                    本帮助
  /v menu     -                 -                    主菜单 (Telegram 用)
  /v confirm  <ID>              -                    二次确认 (危险操作)

详细文档: V4-DESIGN.md`

// ─────────────────────────────────────────────────────────────────────
// /v menu - - - — Telegram main menu (stub)
// ─────────────────────────────────────────────────────────────────────

type MenuHandler struct{}

func (h *MenuHandler) Type() string                           { return "menu" }
func (h *MenuHandler) PrimaryKey(s, p string) (string, error) { return "menu", nil }
func (h *MenuHandler) Validate(cmd *command.Command) error {
	if cmd.Verb != command.VerbView {
		return command.NewError(command.ErrBadFormat, "use /v menu - -")
	}
	return nil
}
func (h *MenuHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v menu")
}
func (h *MenuHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v menu")
}
func (h *MenuHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{
		UserMessage: "💡 在 Telegram 群里发 /menu (不带占位符) 显示带按钮的主菜单。\nCLI 模式下查看完整命令: /v help - -",
	}, nil
}

// ─────────────────────────────────────────────────────────────────────
// /v confirm <ID> - — confirm a pending dangerous action
// ─────────────────────────────────────────────────────────────────────

type ConfirmHandler struct {
	// PendingResolver looks up a pending confirmation by ID and re-issues
	// the original command. Wired by serve.go.
	PendingResolver func(id string) error
}

func (h *ConfirmHandler) Type() string { return "confirm" }
func (h *ConfirmHandler) PrimaryKey(s, p string) (string, error) {
	return "confirm:" + s, nil
}
func (h *ConfirmHandler) Validate(cmd *command.Command) error {
	if cmd.Verb != command.VerbView {
		return command.NewError(command.ErrBadFormat, "use /v confirm <ID> -")
	}
	if command.IsPlaceholder(cmd.Scope) {
		return command.NewError(command.ErrBadFormat, "/v confirm requires <ID> in scope")
	}
	if strings.ContainsAny(cmd.Scope, " \t/") {
		return command.NewError(command.ErrBadFormat, "invalid confirm ID")
	}
	return nil
}
func (h *ConfirmHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v confirm")
}
func (h *ConfirmHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return command.Effects{}, command.NewError(command.ErrBadFormat, "use /v confirm")
}
func (h *ConfirmHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	if h.PendingResolver == nil {
		return command.Effects{}, command.NewError(command.ErrInternal,
			"pending confirmation system not wired")
	}
	if err := h.PendingResolver(cmd.Scope); err != nil {
		return command.Effects{}, err
	}
	return command.Effects{
		UserMessage: fmt.Sprintf("确认完成: %s", cmd.Scope),
	}, nil
}
