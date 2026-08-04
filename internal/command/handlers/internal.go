// Internal peer-add/peer-remove handlers.
//
// These handlers manipulate the peers.json + peers DB table. They're "internal"
// because users don't type them directly — they're emitted by the handshake
// flow (peer-add) and the takeover/leave flow (peer-remove).
//
// Per V4-DESIGN §5.2: peer membership changes go through the config-version
// stream just like any other config change. This guarantees all nodes
// converge on the same peer list.
//
// Command shape:
//
//	/w internal peer-add    <ip>  <join_order>
//	/w internal peer-remove <ip>  -
//	/v internal peer-add    -     -          (no-op; peers viewable via /v nodes)
//
// Type token is `internal` and the operation goes in scope. This keeps
// the strict 4-segment form. Rationale: we already use 'internal' as a
// reserved namespace prefix; rather than create types like
// "internal-peer-add", we collapse the namespace into one type with the
// operation as the scope.
//
// Examples:
//
//	/w internal peer-add    1.2.3.4   2
//	/d internal peer-add    1.2.3.4   -      (alias for peer-remove via mirror symmetry)
//	/w internal peer-remove 1.2.3.4   -
package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/example/meshcdn/internal/command"
	"github.com/example/meshcdn/internal/db"
	"github.com/example/meshcdn/internal/i18n"
	"github.com/example/meshcdn/internal/peers"
)

// InternalHandler dispatches `internal` commands by operation (the scope).
type InternalHandler struct {
	PeerMgr *peers.Manager
}

func (h *InternalHandler) Type() string { return "internal" }

func (h *InternalHandler) PrimaryKey(scope, paramsText string) (string, error) {
	// Primary key for internal commands: "<op>:<target>"
	// e.g. "peer-add:1.2.3.4"
	parts := strings.Fields(paramsText)
	if len(parts) == 0 {
		return scope, nil
	}
	return scope + ":" + parts[0], nil
}

func (h *InternalHandler) Validate(cmd *command.Command) error {
	op := cmd.Scope
	switch op {
	case "peer-add", "peer-remove":
		// params should be "<ip> <join_order>" or "<ip> -"
		fields := strings.Fields(cmd.Params)
		if len(fields) < 1 {
			return command.NewError(command.ErrBadParams,
				fmt.Sprintf("/%s internal %s requires params: <ip> [join_order]",
					cmd.Verb, op))
		}
		if net.ParseIP(fields[0]) == nil {
			return command.NewError(command.ErrBadFormat,
				fmt.Sprintf("invalid IP: %q", fields[0]))
		}
		if op == "peer-add" && cmd.Verb == command.VerbWrite {
			if len(fields) < 2 {
				return command.NewError(command.ErrBadParams,
					"/w internal peer-add requires <ip> <join_order>")
			}
			if _, err := strconv.Atoi(fields[1]); err != nil && fields[1] != "-" {
				return command.NewError(command.ErrBadParams,
					fmt.Sprintf("invalid join_order: %q", fields[1]))
			}
		}
		return nil

	case "set-bot":
		// params: "<ip>" (set override) or "-" (clear). Snapshot-replayable
		// form of /v target; the user-facing validation lives in TargetHandler.
		fields := strings.Fields(cmd.Params)
		if len(fields) < 1 {
			return command.NewError(command.ErrBadParams,
				"/w internal set-bot requires <ip> or -")
		}
		if fields[0] != "-" && net.ParseIP(fields[0]) == nil {
			return command.NewError(command.ErrBadFormat,
				fmt.Sprintf("invalid IP: %q", fields[0]))
		}
		return nil

	case "set-lang":
		// params: "<cn|en>" (set) or "-" (clear → default). Snapshot-replayable
		// form of /v lang; the user-facing messaging lives in LangHandler.
		fields := strings.Fields(cmd.Params)
		if len(fields) < 1 {
			return command.NewError(command.ErrBadParams,
				"/w internal set-lang requires <cn|en> or -")
		}
		if fields[0] != "-" {
			if _, ok := i18n.Parse(fields[0]); !ok {
				return command.NewError(command.ErrBadParams,
					fmt.Sprintf("unsupported language %q (expected cn / en)", fields[0]))
			}
		}
		return nil

	default:
		return command.NewError(command.ErrBadFormat,
			fmt.Sprintf("unknown internal op %q (expected peer-add / peer-remove / set-bot / set-lang)", op))
	}
}

func (h *InternalHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	op := cmd.Scope
	fields := strings.Fields(cmd.Params)

	switch op {
	case "peer-add":
		ip := fields[0]
		var joinOrder int
		if len(fields) >= 2 && fields[1] != "-" {
			joinOrder, _ = strconv.Atoi(fields[1])
		}

		// Update peers.json
		if h.PeerMgr != nil {
			// If join_order specified, use ReplaceAll-style logic; otherwise let
			// PeerMgr auto-assign. For simplicity: just call Add and let it
			// auto-assign; the broadcast-time join_order serves as the canonical
			// source on the originator. Nodes receiving the command via mesh
			// pull will get the auto-assigned value too — should match because
			// they replay from the same snapshot.
			_, err := h.PeerMgr.Add(ip)
			if err != nil {
				return command.Effects{}, fmt.Errorf("peers.Add: %w", err)
			}
		}

		// Update DB peers table (mirror)
		ctx := context.Background()
		_, err := tx.ExecContext(ctx, `
			INSERT INTO peers (ip, join_order)
			VALUES (?, ?)
			ON CONFLICT(ip) DO UPDATE SET join_order = excluded.join_order
		`, ip, joinOrder)
		if err != nil {
			return command.Effects{}, fmt.Errorf("insert peer row: %w", err)
		}

		return command.Effects{
			UserMessage: fmt.Sprintf("peer 已添加: %s (join_order=%d)", ip, joinOrder),
		}, nil

	case "peer-remove":
		ip := fields[0]

		if h.PeerMgr != nil {
			if err := h.PeerMgr.Remove(ip); err != nil {
				return command.Effects{}, fmt.Errorf("peers.Remove: %w", err)
			}
		}

		ctx := context.Background()
		if _, err := tx.ExecContext(ctx, `DELETE FROM peers WHERE ip = ?`, ip); err != nil {
			return command.Effects{}, fmt.Errorf("delete peer row: %w", err)
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("peer 已移除: %s", ip),
		}, nil

	case "set-bot":
		// Persist the explicit bot-node override (or clear it with "-").
		ip := fields[0]
		if ip == "-" {
			ip = ""
		}
		if err := db.SetBotNodeIP(context.Background(), tx, ip); err != nil {
			return command.Effects{}, err
		}
		msg := fmt.Sprintf("bot 节点已设为 %s", ip)
		if ip == "" {
			msg = "bot 节点覆盖已清除 (回退到 join_order 默认)"
		}
		return command.Effects{UserMessage: msg}, nil

	case "set-lang":
		raw := fields[0]
		lang := ""
		if raw != "-" {
			parsed, ok := i18n.Parse(raw)
			if !ok {
				return command.Effects{}, command.NewError(command.ErrBadParams,
					fmt.Sprintf("unsupported language %q", raw))
			}
			lang = string(parsed)
		}
		if err := db.SetLanguage(context.Background(), tx, lang); err != nil {
			return command.Effects{}, err
		}
		shown := lang
		if shown == "" {
			shown = string(i18n.Default)
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("interface language = %s", shown),
		}, nil
	}

	return command.Effects{}, command.NewError(command.ErrBadFormat,
		fmt.Sprintf("unknown internal op %q", op))
}

func (h *InternalHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	// Mirror symmetry: /d internal peer-add <ip> ...  =  remove that peer
	op := cmd.Scope
	switch op {
	case "peer-add", "peer-remove":
		// Both forms mean "remove this peer"
		fields := strings.Fields(cmd.Params)
		if len(fields) < 1 {
			return command.Effects{}, command.NewError(command.ErrBadFormat,
				"requires <ip>")
		}
		ip := fields[0]
		if h.PeerMgr != nil {
			_ = h.PeerMgr.Remove(ip)
		}
		ctx := context.Background()
		if _, err := tx.ExecContext(ctx, `DELETE FROM peers WHERE ip = ?`, ip); err != nil {
			return command.Effects{}, fmt.Errorf("delete peer row: %w", err)
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("peer 已移除: %s", ip),
		}, nil
	}
	return command.Effects{}, command.NewError(command.ErrBadFormat,
		fmt.Sprintf("unknown internal op %q", op))
}

func (h *InternalHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	// Internal commands aren't typically viewed; redirect users to /v nodes.
	return command.Effects{
		UserMessage: "internal commands are not viewable; use /v nodes - - to see peer list",
	}, nil
}
