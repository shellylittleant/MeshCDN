// Bind handler (C-class) — links a domain scope to a rule object.
//
// V4 invariant (per design discussion 2026-04-28):
//
//	The bind scope MUST be the literal display_scope of a row in the domains
//	table. No abbreviations, no port-list expansion: if the domain was
//	created as "https://-:7777,8888", the bind must be written exactly
//	"https://-:7777,8888".
//
// Format:
//
//	/w bind <proto>://<host>:<port-list>  <object-type>:<object-name>
//
// Examples:
//
//	/w bind https://a.com:443       cache:img-7d
//	/w bind https://-:7777,8888     cache:hc001
//	/d bind https://-:317           cache:hc001
//	/v bind - -                     (list all)
//	/v bind https://a.com:443 -     (list bindings for one scope)
//	/v bind - cache:hc001           (list scopes that reference this object)
package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/example/meshcdn/internal/command"
)

type BindHandler struct{}

func (h *BindHandler) Type() string { return "bind" }

func (h *BindHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "bind:" + scope + ":" + paramsText, nil
}

func (h *BindHandler) Validate(cmd *command.Command) error {
	if cmd.Verb == command.VerbView {
		return nil // permissive; full filter logic is in View
	}

	// /w and /d both require a real scope (not "-")
	if command.IsPlaceholder(cmd.Scope) {
		return command.NewError(command.ErrBadFormat,
			"/w bind requires <proto>://<host>:<port-list> in scope (use /v bind - - to list all)")
	}
	// Scope must parse as a domain scope (same syntax as /w domain)
	if _, err := parseDomainScope(cmd.Scope); err != nil {
		return command.NewError(command.ErrBadFormat,
			fmt.Sprintf("invalid bind scope %q: %v", cmd.Scope, err))
	}

	if cmd.Verb == command.VerbWrite || cmd.Verb == command.VerbDelete {
		objType, objName, err := parseObjectRef(cmd.Params)
		if err != nil {
			return command.NewError(command.ErrBadParams, err.Error())
		}
		if objType == "" && cmd.Verb == command.VerbWrite {
			return command.NewError(command.ErrBadParams,
				"params must be <type>:<name> (e.g. cache:img-7d)")
		}
		if objType != "" && !isKnownObjectType(objType) {
			return command.NewError(command.ErrBadParams,
				fmt.Sprintf("unknown object type %q (expected: cache/defense/redirect/header)", objType))
		}
		if objName != "" && !isValidObjectName(objName) {
			return command.NewError(command.ErrBadParams,
				fmt.Sprintf("invalid object name %q", objName))
		}
	}
	return nil
}

func (h *BindHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	objType, objName, _ := parseObjectRef(cmd.Params)
	displayScope := cmd.Scope
	ctx := context.Background()

	// 1. Verify the rule object exists.
	var objExists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rule_objects WHERE type=? AND name=?`,
		objType, objName).Scan(&objExists); err != nil {
		return command.Effects{}, err
	}
	if objExists == 0 {
		return command.Effects{}, command.NewError(command.ErrNotFound,
			fmt.Sprintf("%s 对象 %q 不存在；请先 /w %s %s ...",
				objType, objName, objType, objName))
	}

	// 2. Verify the bind scope is a literal display_scope of an existing domain.
	//    This is the V4 invariant: bind scope must match domain display_scope verbatim.
	var domainExists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM domains WHERE display_scope = ?`,
		displayScope).Scan(&domainExists); err != nil {
		return command.Effects{}, err
	}
	if domainExists == 0 {
		return command.Effects{}, command.NewError(command.ErrNotFound,
			fmt.Sprintf("domain %q 不存在。bind 的 scope 必须与 /w domain 的 scope 字面相同。\n"+
				"用 /v domain - - 查看现有 domain 列表。", displayScope))
	}

	// 3. Insert/update binding (idempotent via UNIQUE)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO bindings (scope, object_type, object_name, config_version, updated_at)
		VALUES (?, ?, ?, (SELECT config_version FROM cluster_meta WHERE id=1) + 1, CURRENT_TIMESTAMP)
		ON CONFLICT(scope, object_type, object_name) DO UPDATE SET
		  config_version = (SELECT config_version FROM cluster_meta WHERE id=1) + 1,
		  updated_at = CURRENT_TIMESTAMP
	`, displayScope, objType, objName); err != nil {
		return command.Effects{}, fmt.Errorf("insert binding: %w", err)
	}

	return command.Effects{
		NeedsNginxReload: true,
		UserMessage: fmt.Sprintf("已绑定: %s → %s:%s",
			displayScope, objType, objName),
	}, nil
}

func (h *BindHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	objType, objName, err := parseObjectRef(cmd.Params)
	if err != nil {
		return command.Effects{}, command.NewError(command.ErrBadParams, err.Error())
	}
	displayScope := cmd.Scope
	ctx := context.Background()

	var res sql.Result
	if command.IsPlaceholder(cmd.Params) {
		// /d bind <scope> -  → remove ALL bindings on this scope
		res, err = tx.ExecContext(ctx,
			`DELETE FROM bindings WHERE scope=?`, displayScope)
	} else {
		res, err = tx.ExecContext(ctx,
			`DELETE FROM bindings WHERE scope=? AND object_type=? AND object_name=?`,
			displayScope, objType, objName)
	}
	if err != nil {
		return command.Effects{}, fmt.Errorf("delete binding: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return command.Effects{
			UserMessage: fmt.Sprintf("%s 上没有匹配的绑定", displayScope),
		}, nil
	}
	return command.Effects{
		NeedsNginxReload: true,
		UserMessage:      fmt.Sprintf("已解除 %d 条绑定", n),
	}, nil
}

func (h *BindHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	ctx := context.Background()
	var sb strings.Builder

	if command.IsPlaceholder(cmd.Scope) && command.IsPlaceholder(cmd.Params) {
		// list all
		rows, err := tx.QueryContext(ctx,
			`SELECT scope, object_type, object_name FROM bindings
			 ORDER BY scope, object_type, object_name`)
		if err != nil {
			return command.Effects{}, err
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var s, ot, on string
			if err := rows.Scan(&s, &ot, &on); err != nil {
				return command.Effects{}, err
			}
			fmt.Fprintf(&sb, "  %s → %s:%s\n", s, ot, on)
			count++
		}
		if count == 0 {
			return command.Effects{UserMessage: "(无绑定)"}, nil
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("绑定关系 (%d 条):\n", count) + sb.String(),
		}, nil
	}

	if !command.IsPlaceholder(cmd.Scope) && command.IsPlaceholder(cmd.Params) {
		// /v bind <scope> -
		rows, err := tx.QueryContext(ctx,
			`SELECT object_type, object_name FROM bindings WHERE scope=?
			 ORDER BY object_type, object_name`, cmd.Scope)
		if err != nil {
			return command.Effects{}, err
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var ot, on string
			if err := rows.Scan(&ot, &on); err != nil {
				return command.Effects{}, err
			}
			fmt.Fprintf(&sb, "  %s:%s\n", ot, on)
			count++
		}
		if count == 0 {
			return command.Effects{UserMessage: fmt.Sprintf("%s 无绑定", cmd.Scope)}, nil
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("%s 的绑定 (%d 条):\n", cmd.Scope, count) + sb.String(),
		}, nil
	}

	if command.IsPlaceholder(cmd.Scope) && !command.IsPlaceholder(cmd.Params) {
		// /v bind - <object>
		objType, objName, err := parseObjectRef(cmd.Params)
		if err != nil {
			return command.Effects{}, command.NewError(command.ErrBadParams, err.Error())
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT scope FROM bindings WHERE object_type=? AND object_name=?
			 ORDER BY scope`, objType, objName)
		if err != nil {
			return command.Effects{}, err
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return command.Effects{}, err
			}
			fmt.Fprintf(&sb, "  %s\n", s)
			count++
		}
		if count == 0 {
			return command.Effects{
				UserMessage: fmt.Sprintf("%s:%s 未被任何 scope 引用", objType, objName),
			}, nil
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("引用 %s:%s 的 scope (%d 个):\n", objType, objName, count) + sb.String(),
		}, nil
	}

	// Both specified: existence check
	objType, objName, err := parseObjectRef(cmd.Params)
	if err != nil {
		return command.Effects{}, command.NewError(command.ErrBadParams, err.Error())
	}
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bindings WHERE scope=? AND object_type=? AND object_name=?`,
		cmd.Scope, objType, objName).Scan(&n); err != nil {
		return command.Effects{}, err
	}
	if n > 0 {
		return command.Effects{
			UserMessage: fmt.Sprintf("✓ 绑定存在: %s → %s:%s", cmd.Scope, objType, objName),
		}, nil
	}
	return command.Effects{
		UserMessage: fmt.Sprintf("✗ 绑定不存在: %s → %s:%s", cmd.Scope, objType, objName),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────

func parseObjectRef(s string) (objType, objName string, err error) {
	if command.IsPlaceholder(s) {
		return "", "", nil
	}
	colon := strings.Index(s, ":")
	if colon <= 0 || colon == len(s)-1 {
		return "", "", fmt.Errorf("expected <type>:<name>, got %q", s)
	}
	return s[:colon], s[colon+1:], nil
}

func isKnownObjectType(t string) bool {
	switch t {
	case "cache", "defense", "redirect", "header":
		return true
	}
	return false
}
