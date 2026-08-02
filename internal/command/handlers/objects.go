// Defense / Redirect / Header rule-object handlers (B-class).
//
// All three follow the same pattern as cache.go:
//   - scope = object name
//   - params = key=value pairs
//   - parsed_json stored alongside text for nginx generator
//   - cascade-delete check via bindings table
//
// Per V4-DESIGN §8.6 (同精度合并规则): cache TTL takes minimum, hsts takes OR,
// defense action favors block, etc. The merging logic lives in the nginx
// precedence generator (precedence.go), not here.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/example/meshcdn/internal/command"
)

// uaReDoSHeuristic flags User-Agent regexes with a quantified group whose body
// itself ends in an unbounded quantifier — the classic catastrophic-backtracking
// shape, e.g. "(a+)+", "(a*)*", "(a+){2,}", "(\d+)+". This is a heuristic, not a
// proof: it deliberately errs toward rejecting the well-known evil forms rather
// than trying to decide ReDoS in general (which is undecidable).
var uaReDoSHeuristic = regexp.MustCompile(`[+*}]\)[*+{]`)

// ─────────────────────────────────────────────────────────────────────
// Defense (block / allow rules)
// ─────────────────────────────────────────────────────────────────────

// DefenseParams: parsed form of a defense rule object.
//
// Keys:
//
//	ip          comma-separated CIDRs / IPs (e.g. "1.2.3.4,10.0.0.0/8")
//	ua          regex for User-Agent matching (Lua-implemented; OpenResty)
//	ref         allowed referer pattern(s), comma-separated
//	action      "block" or "allow", default "block"
//	duration    seconds (0 = permanent)
//	rate        requests-per-second limit (0 = no limit)
//	size        max body size, e.g. "10m" (0 = no limit)
type DefenseParams struct {
	IP       []string `json:"ip,omitempty"`
	UA       string   `json:"ua,omitempty"`
	Ref      []string `json:"ref,omitempty"`
	Action   string   `json:"action,omitempty"`
	Duration int      `json:"duration,omitempty"`
	Rate     int      `json:"rate,omitempty"`
	Size     string   `json:"size,omitempty"`
}

type DefenseHandler struct{}

func (h *DefenseHandler) Type() string { return "defense" }
func (h *DefenseHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "defense:" + scope, nil
}

func (h *DefenseHandler) Validate(cmd *command.Command) error {
	return validateRuleObject(cmd, "defense", validateDefenseParams)
}

func (h *DefenseHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	parsed, err := parseDefenseParams(cmd.Params)
	if err != nil {
		return command.Effects{}, command.NewError(command.ErrBadParams, err.Error())
	}
	return upsertRuleObject(tx, "defense", cmd, parsed,
		fmt.Sprintf("defense 对象 %s 已写入 (action=%s)", cmd.Scope, parsed.Action))
}

func (h *DefenseHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return deleteRuleObject(tx, "defense", cmd.Scope)
}

func (h *DefenseHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return viewRuleObject(tx, "defense", cmd, func(text string) (interface{}, error) {
		return parseDefenseParams(text)
	})
}

func parseDefenseParams(text string) (*DefenseParams, error) {
	m, err := command.ParseKeyValueParams(text)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(m, "defense", "ip", "ua", "ref", "action", "duration", "rate", "size"); err != nil {
		return nil, err
	}
	out := &DefenseParams{Action: "block"} // default

	if v, ok := m["ip"]; ok && v != "" {
		out.IP = splitCSV(v)
	}
	if v, ok := m["ua"]; ok {
		if err := validateUARegex(v); err != nil {
			return nil, err
		}
		out.UA = v
	}
	if v, ok := m["ref"]; ok && v != "" {
		out.Ref = splitCSV(v)
	}
	if v, ok := m["action"]; ok {
		if v != "block" && v != "allow" {
			return nil, fmt.Errorf("action must be block or allow, got %q", v)
		}
		out.Action = v
	}
	if v, ok := m["duration"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid duration: %q", v)
		}
		out.Duration = n
	}
	if v, ok := m["rate"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid rate: %q", v)
		}
		out.Rate = n
	}
	if v, ok := m["size"]; ok {
		out.Size = v // we accept "10m", "1g", etc; nginx parses for us
	}

	if len(out.IP) == 0 && out.UA == "" && len(out.Ref) == 0 && out.Rate == 0 && out.Size == "" {
		return nil, fmt.Errorf("defense object needs at least one criterion (ip/ua/ref/rate/size)")
	}
	return out, nil
}

func validateDefenseParams(cmd *command.Command) error {
	if cmd.Verb == command.VerbWrite {
		_, err := parseDefenseParams(cmd.Params)
		if err != nil {
			return command.NewError(command.ErrBadParams, err.Error())
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────
// Redirect
// ─────────────────────────────────────────────────────────────────────

// RedirectParams: parsed form of a redirect rule.
//
// Keys:
//
//	from   path or pattern to match, e.g. "/old", "/old/(.*)" (regex if regex=true)
//	to     destination URL or path, may include $1 backref or {host}{uri}
//	status HTTP status, default 301
//	regex  "true"/"false", default false (literal prefix match)
type RedirectParams struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Status int    `json:"status,omitempty"`
	Regex  bool   `json:"regex,omitempty"`
}

type RedirectHandler struct{}

func (h *RedirectHandler) Type() string { return "redirect" }
func (h *RedirectHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "redirect:" + scope, nil
}

func (h *RedirectHandler) Validate(cmd *command.Command) error {
	return validateRuleObject(cmd, "redirect", validateRedirectParams)
}

func (h *RedirectHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	parsed, err := parseRedirectParams(cmd.Params)
	if err != nil {
		return command.Effects{}, command.NewError(command.ErrBadParams, err.Error())
	}
	return upsertRuleObject(tx, "redirect", cmd, parsed,
		fmt.Sprintf("redirect 对象 %s 已写入 (%s → %s)", cmd.Scope, parsed.From, parsed.To))
}

func (h *RedirectHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return deleteRuleObject(tx, "redirect", cmd.Scope)
}

func (h *RedirectHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return viewRuleObject(tx, "redirect", cmd, func(text string) (interface{}, error) {
		return parseRedirectParams(text)
	})
}

func parseRedirectParams(text string) (*RedirectParams, error) {
	m, err := command.ParseKeyValueParams(text)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(m, "redirect", "from", "to", "status", "regex"); err != nil {
		return nil, err
	}
	out := &RedirectParams{Status: 301}
	if v, ok := m["from"]; ok {
		out.From = v
	} else {
		return nil, fmt.Errorf("redirect requires from=...")
	}
	if v, ok := m["to"]; ok {
		out.To = v
	} else {
		return nil, fmt.Errorf("redirect requires to=...")
	}
	if v, ok := m["status"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 300 || n >= 400 {
			return nil, fmt.Errorf("invalid redirect status: %q (expected 3xx)", v)
		}
		out.Status = n
	}
	if v, ok := m["regex"]; ok {
		out.Regex = (v == "true")
	}
	return out, nil
}

func validateRedirectParams(cmd *command.Command) error {
	if cmd.Verb == command.VerbWrite {
		_, err := parseRedirectParams(cmd.Params)
		if err != nil {
			return command.NewError(command.ErrBadParams, err.Error())
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────
// Header
// ─────────────────────────────────────────────────────────────────────

// HeaderParams: parsed form of a header rule.
//
// A single rule object can declare multiple add/remove operations.
//
// Keys (all comma-separated):
//
//	request_add        "Name=value, Other=val"
//	request_del        "Name1, Name2"
//	response_add       "Name=value, Other=val"
//	response_del       "Name1, Name2"
type HeaderParams struct {
	RequestAdd  map[string]string `json:"request_add,omitempty"`
	RequestDel  []string          `json:"request_del,omitempty"`
	ResponseAdd map[string]string `json:"response_add,omitempty"`
	ResponseDel []string          `json:"response_del,omitempty"`
}

type HeaderHandler struct{}

func (h *HeaderHandler) Type() string { return "header" }
func (h *HeaderHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "header:" + scope, nil
}

func (h *HeaderHandler) Validate(cmd *command.Command) error {
	return validateRuleObject(cmd, "header", validateHeaderParams)
}

func (h *HeaderHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	parsed, err := parseHeaderParams(cmd.Params)
	if err != nil {
		return command.Effects{}, command.NewError(command.ErrBadParams, err.Error())
	}
	return upsertRuleObject(tx, "header", cmd, parsed,
		fmt.Sprintf("header 对象 %s 已写入", cmd.Scope))
}

func (h *HeaderHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return deleteRuleObject(tx, "header", cmd.Scope)
}

func (h *HeaderHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	return viewRuleObject(tx, "header", cmd, func(text string) (interface{}, error) {
		return parseHeaderParams(text)
	})
}

func parseHeaderParams(text string) (*HeaderParams, error) {
	m, err := command.ParseKeyValueParams(text)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(m, "header", "request_add", "request_del", "response_add", "response_del"); err != nil {
		return nil, err
	}
	out := &HeaderParams{}

	parseKVList := func(s string) map[string]string {
		result := make(map[string]string)
		for _, pair := range splitCSV(s) {
			eq := strings.Index(pair, "=")
			if eq <= 0 {
				continue
			}
			result[strings.TrimSpace(pair[:eq])] = strings.TrimSpace(pair[eq+1:])
		}
		return result
	}

	if v, ok := m["request_add"]; ok {
		out.RequestAdd = parseKVList(v)
	}
	if v, ok := m["request_del"]; ok {
		out.RequestDel = splitCSV(v)
	}
	if v, ok := m["response_add"]; ok {
		out.ResponseAdd = parseKVList(v)
	}
	if v, ok := m["response_del"]; ok {
		out.ResponseDel = splitCSV(v)
	}

	if len(out.RequestAdd) == 0 && len(out.RequestDel) == 0 &&
		len(out.ResponseAdd) == 0 && len(out.ResponseDel) == 0 {
		return nil, fmt.Errorf("header object needs at least one of request_add/request_del/response_add/response_del")
	}
	return out, nil
}

func validateHeaderParams(cmd *command.Command) error {
	if cmd.Verb == command.VerbWrite {
		_, err := parseHeaderParams(cmd.Params)
		if err != nil {
			return command.NewError(command.ErrBadParams, err.Error())
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────
// Common helpers shared by all B-class rule objects
// ─────────────────────────────────────────────────────────────────────

// validateRuleObject is shared validation logic.
func validateRuleObject(cmd *command.Command, typeName string,
	specialValidate func(cmd *command.Command) error) error {

	if cmd.Verb == command.VerbView {
		return nil
	}
	if command.IsPlaceholder(cmd.Scope) {
		return command.NewError(command.ErrBadFormat,
			fmt.Sprintf("/w %s requires <n> in scope", typeName))
	}
	if !isValidObjectName(cmd.Scope) {
		return command.NewError(command.ErrBadFormat,
			fmt.Sprintf("invalid %s object name %q", typeName, cmd.Scope))
	}
	if cmd.Verb == command.VerbWrite {
		return specialValidate(cmd)
	}
	return nil
}

// upsertRuleObject inserts/updates a rule_objects row.
func upsertRuleObject(tx *sql.Tx, typeName string, cmd *command.Command,
	parsed interface{}, msg string) (command.Effects, error) {

	parsedJSON, _ := json.Marshal(parsed)
	ctx := context.Background()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO rule_objects (type, name, params_text, parsed_json, config_version, updated_at)
		VALUES (?, ?, ?, ?, (SELECT config_version FROM cluster_meta WHERE id=1) + 1, CURRENT_TIMESTAMP)
		ON CONFLICT(type, name) DO UPDATE SET
		  params_text = excluded.params_text,
		  parsed_json = excluded.parsed_json,
		  config_version = (SELECT config_version FROM cluster_meta WHERE id=1) + 1,
		  updated_at = CURRENT_TIMESTAMP
	`, typeName, cmd.Scope, cmd.Params, string(parsedJSON))
	if err != nil {
		return command.Effects{}, fmt.Errorf("upsert %s: %w", typeName, err)
	}
	return command.Effects{
		NeedsNginxReload: true,
		UserMessage:      msg,
	}, nil
}

// deleteRuleObject implements the cascade-protected delete.
func deleteRuleObject(tx *sql.Tx, typeName, name string) (command.Effects, error) {
	ctx := context.Background()

	rows, err := tx.QueryContext(ctx,
		`SELECT scope FROM bindings WHERE object_type=? AND object_name=? ORDER BY scope`,
		typeName, name)
	if err != nil {
		return command.Effects{}, err
	}
	var refs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			refs = append(refs, s)
		}
	}
	rows.Close()

	if len(refs) > 0 {
		return command.Effects{}, command.NewError(command.ErrCascadeBlocked,
			fmt.Sprintf("%s 对象 %q 仍被以下绑定引用: %s\n请先解除绑定: /d bind <scope> %s:%s",
				typeName, name, strings.Join(refs, ", "), typeName, name))
	}

	res, err := tx.ExecContext(ctx,
		`DELETE FROM rule_objects WHERE type=? AND name=?`, typeName, name)
	if err != nil {
		return command.Effects{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return command.Effects{
			UserMessage: fmt.Sprintf("%s 对象 %q 不存在", typeName, name),
		}, nil
	}
	return command.Effects{
		NeedsNginxReload: true,
		UserMessage:      fmt.Sprintf("%s 对象 %q 已删除", typeName, name),
	}, nil
}

// viewRuleObject is the shared view implementation.
func viewRuleObject(tx *sql.Tx, typeName string, cmd *command.Command,
	parser func(string) (interface{}, error)) (command.Effects, error) {

	ctx := context.Background()
	var sb strings.Builder

	if command.IsPlaceholder(cmd.Scope) {
		rows, err := tx.QueryContext(ctx,
			`SELECT name, params_text FROM rule_objects WHERE type=? ORDER BY name`,
			typeName)
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
		if count == 0 {
			return command.Effects{UserMessage: fmt.Sprintf("(无 %s 对象)", typeName)}, nil
		}
		header := fmt.Sprintf("%s 对象 (%d 个):\n", typeName, count)
		return command.Effects{UserMessage: header + sb.String()}, nil
	}

	var name, params string
	err := tx.QueryRowContext(ctx,
		`SELECT name, params_text FROM rule_objects WHERE type=? AND name=?`,
		typeName, cmd.Scope,
	).Scan(&name, &params)
	if err == sql.ErrNoRows {
		return command.Effects{
			UserMessage: fmt.Sprintf("%s 对象 %q 不存在", typeName, cmd.Scope),
		}, nil
	}
	if err != nil {
		return command.Effects{}, err
	}

	fmt.Fprintf(&sb, "%s 对象 %s\n", typeName, name)
	fmt.Fprintf(&sb, "  原始: %s\n", params)
	if parsed, err := parser(params); err == nil {
		js, _ := json.MarshalIndent(parsed, "  ", "  ")
		fmt.Fprintf(&sb, "  解析: %s\n", string(js))
	}

	bindRows, err := tx.QueryContext(ctx,
		`SELECT scope FROM bindings WHERE object_type=? AND object_name=? ORDER BY scope`,
		typeName, cmd.Scope)
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

// rejectUnknownKeys fails a B-class object parse when it carries a key the
// handler does not understand. Previously unknown keys were silently dropped,
// so a typo (or an as-yet-unimplemented feature like geo=/cc=) would "succeed"
// while doing nothing — a silent-failure trap. geo/cc get a dedicated message
// because they appear in older docs but are not implemented anywhere.
func rejectUnknownKeys(m map[string]string, typeName string, allowed ...string) error {
	allow := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		allow[k] = true
	}
	for k := range m {
		if allow[k] {
			continue
		}
		switch k {
		case "geo", "cc":
			return fmt.Errorf("%s: 参数 %q 尚未实现（geo 地理封禁 / cc 防护仍在路线图待定区），请勿使用以免误以为已生效", typeName, k)
		default:
			return fmt.Errorf("%s: 未知参数 %q（允许: %s）", typeName, k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// validateUARegex rejects User-Agent patterns that are unsafe to compile into
// the OpenResty access_by_lua_block. The pattern is always embedded as a Lua
// *string* (never as code) and matched with ngx.re.match, but we still sanitize
// defensively: length bound, no control characters, no catastrophic-backtracking
// shape, and it must be a syntactically valid regexp. Rejecting here (Validate
// stage) means a bad pattern never reaches nginx config generation.
func validateUARegex(pat string) error {
	if pat == "" {
		return nil
	}
	if len(pat) > 256 {
		return fmt.Errorf("ua 正则过长（%d > 256 字符）", len(pat))
	}
	for _, r := range pat {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("ua 正则包含控制字符 %U，已拒绝", r)
		}
	}
	if uaReDoSHeuristic.MatchString(pat) {
		return fmt.Errorf("ua 正则 %q 具有灾难性回溯（ReDoS）结构，请拆解嵌套量词后重试", pat)
	}
	// RE2 has no backtracking constructs, so a successful compile here also rules
	// out backreferences/lookaround — additional ReDoS-vector protection. PCRE-only
	// constructs are rare in UA matching and rejecting them is the safe default.
	if _, err := regexp.Compile(pat); err != nil {
		return fmt.Errorf("ua 正则 %q 非法: %v", pat, err)
	}
	return nil
}

// splitCSV splits "a, b, c" → ["a", "b", "c"], trimming whitespace.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
