// Cache rule-object handler (B-class).
//
// Per V4-DESIGN §8.3:
//
//	/w cache <name> <key=value...>
//
// Example:
//
//	/w cache img-7d patterns=*.jpg,*.png ttl=604800 hsts=true
//
// scope = object name (just an identifier — not a domain).
// params = key=value pairs space-separated.
//
// Supported keys:
//
//	patterns  comma-separated list of glob patterns or path prefixes
//	          (*.jpg, /static/, ^~ /assets/, ~ ^/api/v[0-9]+/, = /exact)
//	ttl       seconds, integer ≥ 0; 0 = "do not cache"
//	browser_ttl  seconds, optional; defaults to ttl if absent
//	hsts      "true" / "false", default false
//
// Mirror symmetry: /d cache <name> - removes the object (and cascades to
// any /w bind references — controlled by V4-DESIGN §8.4 "/confirm cascade").
//
// Step 6 simplification: cascade prompt is implemented inline (returns
// CONFIRM_REQUIRED if bind references exist). The step-6 PendingConfirmStore
// stores the original /d command; user replies /v confirm <ID> - to force.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/example/meshcdn/internal/command"
)

// CacheParams is the parsed form of cache rule params (a single object's
// content). Stored in db.rule_objects.parsed_json so the nginx generator
// can read it without re-parsing the original text.
type CacheParams struct {
	Patterns   []string `json:"patterns"`
	TTL        int      `json:"ttl"`
	BrowserTTL int      `json:"browser_ttl,omitempty"`
	HSTS       bool     `json:"hsts,omitempty"`
}

// CacheHandler implements command.Handler for "cache" type.
type CacheHandler struct {
	// Step 7 will add PendingAdder for the /v confirm cascade-delete flow.
	// Step 6 returns ErrCascadeBlocked instead, requiring user to manually
	// /d bind first.
}

func (h *CacheHandler) Type() string { return "cache" }

func (h *CacheHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "cache:" + scope, nil
}

func (h *CacheHandler) Validate(cmd *command.Command) error {
	if cmd.Verb == command.VerbView {
		return nil // very permissive: /v cache - - lists all
	}
	if command.IsPlaceholder(cmd.Scope) {
		return command.NewError(command.ErrBadFormat,
			"/w cache requires <name> in scope")
	}
	if !isValidObjectName(cmd.Scope) {
		return command.NewError(command.ErrBadFormat,
			fmt.Sprintf("invalid object name %q", cmd.Scope))
	}

	if cmd.Verb == command.VerbWrite {
		// Must have at least patterns
		params, err := command.ParseKeyValueParams(cmd.Params)
		if err != nil {
			return err
		}
		patternsStr, ok := params["patterns"]
		if !ok || patternsStr == "" {
			return command.NewError(command.ErrBadParams,
				"/w cache requires patterns=...")
		}
		// ttl validation
		if ttlStr, ok := params["ttl"]; ok {
			if v, err := strconv.Atoi(ttlStr); err != nil || v < 0 {
				return command.NewError(command.ErrBadParams,
					fmt.Sprintf("invalid ttl: %q", ttlStr))
			}
		}
		if btlStr, ok := params["browser_ttl"]; ok {
			if v, err := strconv.Atoi(btlStr); err != nil || v < 0 {
				return command.NewError(command.ErrBadParams,
					fmt.Sprintf("invalid browser_ttl: %q", btlStr))
			}
		}
		if hstsStr, ok := params["hsts"]; ok {
			if hstsStr != "true" && hstsStr != "false" {
				return command.NewError(command.ErrBadParams,
					"hsts must be true or false")
			}
		}
	}
	return nil
}

func (h *CacheHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	parsed, err := parseCacheParams(cmd.Params)
	if err != nil {
		return command.Effects{}, command.NewError(command.ErrBadParams, err.Error())
	}
	parsedJSON, _ := json.Marshal(parsed)

	ctx := context.Background()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO rule_objects (type, name, params_text, parsed_json, config_version, updated_at)
		VALUES ('cache', ?, ?, ?, (SELECT config_version FROM cluster_meta WHERE id=1) + 1, CURRENT_TIMESTAMP)
		ON CONFLICT(type, name) DO UPDATE SET
		  params_text = excluded.params_text,
		  parsed_json = excluded.parsed_json,
		  config_version = (SELECT config_version FROM cluster_meta WHERE id=1) + 1,
		  updated_at = CURRENT_TIMESTAMP
	`, cmd.Scope, cmd.Params, string(parsedJSON))
	if err != nil {
		return command.Effects{}, fmt.Errorf("upsert cache object: %w", err)
	}

	// Identify domains affected (transitively, via bindings) so nginx
	// regen knows to recompute their precedence trees. For simplicity we
	// just trigger a full reload — the generator inspects all bindings
	// regardless.
	return command.Effects{
		NeedsNginxReload: true,
		UserMessage: fmt.Sprintf("缓存对象 %s 已写入 (%d patterns, ttl=%ds)",
			cmd.Scope, len(parsed.Patterns), parsed.TTL),
	}, nil
}

func (h *CacheHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	ctx := context.Background()

	// Check binding references
	var bindCount int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bindings WHERE object_type='cache' AND object_name=?`,
		cmd.Scope,
	).Scan(&bindCount)
	if err != nil {
		return command.Effects{}, err
	}

	if bindCount > 0 {
		// Step 6 cascade-delete-with-confirm flow:
		//
		// We DON'T delete now. Instead, we ask user to /v confirm <ID> -
		// which (when wired to a "force-cascade" path) will re-execute
		// this delete with cascade flag set.
		//
		// In step 6, we keep this simple: just return a clear error
		// listing which bindings exist. User must manually /d bind first,
		// then retry /d cache.
		var refs []string
		rows, err := tx.QueryContext(ctx,
			`SELECT scope FROM bindings WHERE object_type='cache' AND object_name=? ORDER BY scope`,
			cmd.Scope)
		if err != nil {
			return command.Effects{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return command.Effects{}, err
			}
			refs = append(refs, s)
		}
		return command.Effects{}, command.NewError(command.ErrCascadeBlocked,
			fmt.Sprintf("cache 对象 %q 仍被以下绑定引用: %s\n请先解除绑定: /d bind <scope> cache:%s",
				cmd.Scope, strings.Join(refs, ", "), cmd.Scope))
	}

	res, err := tx.ExecContext(ctx,
		`DELETE FROM rule_objects WHERE type='cache' AND name=?`, cmd.Scope)
	if err != nil {
		return command.Effects{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return command.Effects{
			UserMessage: fmt.Sprintf("cache 对象 %q 不存在", cmd.Scope),
		}, nil
	}
	return command.Effects{
		NeedsNginxReload: true,
		UserMessage:      fmt.Sprintf("cache 对象 %q 已删除", cmd.Scope),
	}, nil
}

func (h *CacheHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	ctx := context.Background()
	var sb strings.Builder

	if command.IsPlaceholder(cmd.Scope) {
		// list all
		rows, err := tx.QueryContext(ctx,
			`SELECT name, params_text FROM rule_objects WHERE type='cache' ORDER BY name`)
		if err != nil {
			return command.Effects{}, err
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var name, params string
			if err := rows.Scan(&name, &params); err != nil {
				return command.Effects{}, err
			}
			fmt.Fprintf(&sb, "  %-20s  %s\n", name, params)
			count++
		}
		header := fmt.Sprintf("cache 对象 (%d 个):\n", count)
		if count == 0 {
			return command.Effects{UserMessage: "(无 cache 对象)"}, nil
		}
		return command.Effects{UserMessage: header + sb.String()}, nil
	}

	// view single
	var name, params string
	err := tx.QueryRowContext(ctx,
		`SELECT name, params_text FROM rule_objects WHERE type='cache' AND name=?`,
		cmd.Scope,
	).Scan(&name, &params)
	if err == sql.ErrNoRows {
		return command.Effects{
			UserMessage: fmt.Sprintf("cache 对象 %q 不存在", cmd.Scope),
		}, nil
	}
	if err != nil {
		return command.Effects{}, err
	}

	parsed, _ := parseCacheParams(params)
	fmt.Fprintf(&sb, "cache 对象 %s\n", name)
	fmt.Fprintf(&sb, "  原始: %s\n", params)
	fmt.Fprintf(&sb, "  解析:\n")
	fmt.Fprintf(&sb, "    patterns:    %v\n", parsed.Patterns)
	fmt.Fprintf(&sb, "    ttl:         %ds\n", parsed.TTL)
	if parsed.BrowserTTL > 0 {
		fmt.Fprintf(&sb, "    browser_ttl: %ds\n", parsed.BrowserTTL)
	}
	fmt.Fprintf(&sb, "    hsts:        %v\n", parsed.HSTS)

	// Also show which domains reference it
	bindRows, err := tx.QueryContext(ctx,
		`SELECT scope FROM bindings WHERE object_type='cache' AND object_name=? ORDER BY scope`,
		cmd.Scope)
	if err == nil {
		defer bindRows.Close()
		var refs []string
		for bindRows.Next() {
			var s string
			if err := bindRows.Scan(&s); err == nil {
				refs = append(refs, s)
			}
		}
		if len(refs) > 0 {
			fmt.Fprintf(&sb, "  绑定到: %s\n", strings.Join(refs, ", "))
		} else {
			fmt.Fprintf(&sb, "  绑定到: (未绑定)\n")
		}
	}

	return command.Effects{UserMessage: sb.String()}, nil
}

// ─────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────

// parseCacheParams takes a key=value... text and returns the structured form.
func parseCacheParams(text string) (*CacheParams, error) {
	m, err := command.ParseKeyValueParams(text)
	if err != nil {
		return nil, err
	}
	out := &CacheParams{}

	if patterns, ok := m["patterns"]; ok && patterns != "" {
		for _, p := range strings.Split(patterns, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out.Patterns = append(out.Patterns, p)
			}
		}
	}
	if ttlStr, ok := m["ttl"]; ok {
		v, err := strconv.Atoi(ttlStr)
		if err != nil {
			return nil, fmt.Errorf("invalid ttl: %v", err)
		}
		out.TTL = v
	}
	if btlStr, ok := m["browser_ttl"]; ok {
		v, err := strconv.Atoi(btlStr)
		if err != nil {
			return nil, fmt.Errorf("invalid browser_ttl: %v", err)
		}
		out.BrowserTTL = v
	}
	if hstsStr, ok := m["hsts"]; ok {
		out.HSTS = (hstsStr == "true")
	}
	return out, nil
}

// isValidObjectName reports whether s is a valid identifier for a rule object.
// Allows letters, digits, underscores, hyphens. 1-64 chars.
func isValidObjectName(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			// ok
		default:
			return false
		}
	}
	return true
}
