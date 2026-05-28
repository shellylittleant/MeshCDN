// Domain command handler (A-class).
//
// V4 invariant (per design discussion 2026-04-28):
//
//  1. A scope's identity is the literal display string the user wrote
//     (e.g. "https://-:7777,8888"). Two writes with the SAME literal string
//     = update origin in place. Different literal strings = different scopes.
//
//  2. No two scopes may share any port number on the same node. If user
//     tries /w domain https://-:7777 while another scope already covers 7777
//     (e.g. https://-:7777,8888), the write is REJECTED with ErrPortConflict.
//     User must /d domain the existing scope first.
//
//  3. Storage: each domain row covers exactly one (proto, host, port) tuple.
//     A multi-port write expands into N rows that share the same display_scope
//     string. /v export and /v domain dedupe by display_scope to show the
//     original user form.
//
//  4. Bindings (cache/defense/redirect/header) attach to display_scope verbatim.
//     Modifying domain's display_scope (changing port list) requires deleting
//     first, which cascades into bindings (or is blocked if bindings exist).
//
// Command shape:
//
//	/w domain <proto>://<host>:<port-list>   <origin>
//	/d domain <proto>://<host>:<port-list>   - (or anything; ignored)
//	/v domain <proto>://<host>:<port-list>   -
//	/v domain -                              - (list all)
package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/example/meshcdn/internal/command"
	"github.com/example/meshcdn/internal/db"
)

// DomainHandler implements command.Handler for the "domain" type.
type DomainHandler struct{}

func (h *DomainHandler) Type() string { return "domain" }

// PrimaryKey for domain rules: the literal scope string.
// Two writes with same literal scope = update.
func (h *DomainHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return scope, nil
}

func (h *DomainHandler) Validate(cmd *command.Command) error {
	switch cmd.Verb {
	case command.VerbView:
		if !command.IsPlaceholder(cmd.Scope) {
			if _, err := parseDomainScope(cmd.Scope); err != nil {
				return command.NewError(command.ErrBadFormat,
					fmt.Sprintf("invalid scope: %v", err))
			}
		}
		if !command.IsPlaceholder(cmd.Params) {
			return command.NewError(command.ErrBadParams,
				"/v domain currently expects params = -")
		}
		return nil

	case command.VerbWrite, command.VerbDelete:
		if command.IsPlaceholder(cmd.Scope) {
			return command.NewError(command.ErrBadFormat,
				"/w/d domain requires a scope (use https://-:port for port fallback)")
		}
		if _, err := parseDomainScope(cmd.Scope); err != nil {
			return command.NewError(command.ErrBadFormat,
				fmt.Sprintf("invalid scope: %v", err))
		}
		if cmd.Verb == command.VerbWrite {
			if cmd.Params == "" {
				return command.NewError(command.ErrBadParams,
					"/w domain requires origin (use - for welcome page)")
			}
			if !command.IsPlaceholder(cmd.Params) {
				if _, err := parseOriginEndpoint(cmd.Params); err != nil {
					return command.NewError(command.ErrBadParams,
						fmt.Sprintf("invalid origin: %v", err))
				}
			}
		}
		return nil
	}
	return command.NewError(command.ErrBadFormat, "unknown verb")
}

// Write — see V4 invariant 1, 2, 3 in the file header comment.
func (h *DomainHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	scope, err := parseDomainScope(cmd.Scope)
	if err != nil {
		return command.Effects{}, command.NewError(command.ErrBadFormat, err.Error())
	}

	// Canonical display_scope: use the user's input literally. We preserve
	// it byte-for-byte so /v export round-trips exactly.
	displayScope := cmd.Scope
	ctx := context.Background()

	// Port conflict check (invariant 2):
	// For each port in the requested scope, look up which display_scope
	// currently owns it. If the owner is different from our display_scope,
	// reject. If equal, this is an update — fine.
	for _, port := range scope.Ports {
		canon := canonicalScope(scope.Proto, scope.Host, port)
		var existingDisplay sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT display_scope FROM domains WHERE scope = ?`,
			canon).Scan(&existingDisplay)
		if err == sql.ErrNoRows {
			continue // free port
		}
		if err != nil {
			return command.Effects{}, fmt.Errorf("check port conflict: %w", err)
		}
		if existingDisplay.Valid && existingDisplay.String != displayScope {
			return command.Effects{}, command.NewError(command.ErrPortConflict,
				fmt.Sprintf("端口 %d 已被另一个 scope 占用: %s。请先 /d domain %s -",
					port, existingDisplay.String, existingDisplay.String))
		}
	}

	// Insert/update each port row with this display_scope.
	bindings := make([]command.PortProtocolBinding, 0, len(scope.Ports))
	for _, port := range scope.Ports {
		canon := canonicalScope(scope.Proto, scope.Host, port)
		_, err := tx.ExecContext(ctx,
			`INSERT INTO domains (scope, display_scope, origin, config_version, updated_at)
			 VALUES (?, ?, ?, (SELECT config_version FROM cluster_meta WHERE id=1) + 1, CURRENT_TIMESTAMP)
			 ON CONFLICT(scope) DO UPDATE SET
			   display_scope = excluded.display_scope,
			   origin = excluded.origin,
			   config_version = (SELECT config_version FROM cluster_meta WHERE id=1) + 1,
			   updated_at = CURRENT_TIMESTAMP`,
			canon, displayScope, cmd.Params,
		)
		if err != nil {
			return command.Effects{}, fmt.Errorf("insert domain row: %w", err)
		}
		bindings = append(bindings, command.PortProtocolBinding{
			Port:     port,
			Protocol: scope.Proto,
		})
	}

	hostLabel := scope.Host
	if hostLabel == "-" {
		hostLabel = "any-host"
	}

	return command.Effects{
		NeedsNginxReload:     true,
		PortProtocolBindings: bindings,
		UserMessage: fmt.Sprintf("写入域名规则: %s on %d port(s) → %s",
			hostLabel, len(scope.Ports), cmd.Params),
	}, nil
}

// Delete — invariant 4: blocks if any binding still references this display_scope.
func (h *DomainHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	scope, err := parseDomainScope(cmd.Scope)
	if err != nil {
		return command.Effects{}, command.NewError(command.ErrBadFormat, err.Error())
	}
	ctx := context.Background()
	displayScope := cmd.Scope

	// Verify the rows we're about to delete actually share this display_scope
	// (i.e. user is naming the scope by its literal form, not piecemeal).
	// Collect the canonical scopes that match our (proto, host, ports).
	canonScopes := make([]string, 0, len(scope.Ports))
	for _, port := range scope.Ports {
		canonScopes = append(canonScopes, canonicalScope(scope.Proto, scope.Host, port))
	}

	// For each canonical scope, check the row's display_scope matches.
	// If any port's display_scope differs, the user gave us an inconsistent
	// scope (e.g. asked to delete "https://-:7777" but that port is part of
	// a multi-port scope "https://-:7777,8888"). Reject.
	for i, canon := range canonScopes {
		var stored sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT display_scope FROM domains WHERE scope = ?`, canon).Scan(&stored)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return command.Effects{}, fmt.Errorf("check display_scope: %w", err)
		}
		if stored.Valid && stored.String != displayScope {
			return command.Effects{}, command.NewError(command.ErrBadFormat,
				fmt.Sprintf("端口 %d 属于另一个 scope: %s。请用完整 scope 删除: /d domain %s -",
					scope.Ports[i], stored.String, stored.String))
		}
	}

	// Cascade check: any bindings on this display_scope?
	var bindCount int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bindings WHERE scope = ?`, displayScope).Scan(&bindCount)
	if err != nil {
		return command.Effects{}, fmt.Errorf("check bindings: %w", err)
	}
	if bindCount > 0 {
		return command.Effects{}, command.NewError(command.ErrCascadeBlocked,
			fmt.Sprintf("domain %s 仍被 %d 条 binding 引用，请先 /d bind %s ... 解除绑定",
				displayScope, bindCount, displayScope))
	}

	// All clear — delete the rows.
	deleted := 0
	for _, canon := range canonScopes {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM domains WHERE scope = ? AND display_scope = ?`,
			canon, displayScope)
		if err != nil {
			return command.Effects{}, fmt.Errorf("delete domain: %w", err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
	}

	if deleted == 0 {
		return command.Effects{
			UserMessage: fmt.Sprintf("未找到匹配的域名规则: %s", displayScope),
		}, nil
	}
	return command.Effects{
		NeedsNginxReload: true,
		UserMessage:      fmt.Sprintf("删除域名规则: %s (%d 端口)", displayScope, deleted),
	}, nil
}

// View — list domains, deduplicated by display_scope.
func (h *DomainHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	ctx := context.Background()

	// Always group by display_scope so output matches user's input form.
	// Pick MIN(origin) to represent — all rows under same display_scope share origin anyway.
	var rows *sql.Rows
	var err error
	if command.IsPlaceholder(cmd.Scope) {
		rows, err = tx.QueryContext(ctx,
			`SELECT display_scope, MIN(origin), MAX(config_version), MAX(updated_at)
			 FROM domains GROUP BY display_scope ORDER BY display_scope`)
	} else {
		// Filter by display_scope literal match
		rows, err = tx.QueryContext(ctx,
			`SELECT display_scope, MIN(origin), MAX(config_version), MAX(updated_at)
			 FROM domains WHERE display_scope = ?
			 GROUP BY display_scope`, cmd.Scope)
	}
	if err != nil {
		return command.Effects{}, fmt.Errorf("query domains: %w", err)
	}
	defer rows.Close()

	var sb strings.Builder
	count := 0
	for rows.Next() {
		var ds, origin, updatedAt string
		var version int64
		if err := rows.Scan(&ds, &origin, &version, &updatedAt); err != nil {
			return command.Effects{}, err
		}
		fmt.Fprintf(&sb, "  %s → %s   (v%d, %s)\n", ds, origin, version, updatedAt)
		count++
	}
	if count == 0 {
		return command.Effects{UserMessage: "(无匹配的域名规则)"}, nil
	}
	header := fmt.Sprintf("域名规则 (%d 条):\n", count)
	return command.Effects{UserMessage: header + sb.String()}, nil
}

// ─────────────────────────────────────────────────────────────────────
// Scope parsing
// ─────────────────────────────────────────────────────────────────────

type domainScope struct {
	Proto string
	Host  string
	Ports []int // sorted ascending, deduped
}

var scopeRE = regexp.MustCompile(`^(https?)://([a-zA-Z0-9\-\.]+|-)(?::([\d,]+))?$`)

func parseDomainScope(s string) (*domainScope, error) {
	m := scopeRE.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("scope must be <proto>://<host>:<port>, got %q", s)
	}
	proto := m[1]
	host := m[2]
	portStr := m[3]

	if portStr == "" {
		return nil, errors.New("scope must include a port")
	}
	portStrs := strings.Split(portStr, ",")
	ports := make([]int, 0, len(portStrs))
	seen := make(map[int]bool)
	for _, ps := range portStrs {
		p, err := strconv.Atoi(ps)
		if err != nil {
			return nil, fmt.Errorf("invalid port %q", ps)
		}
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("port out of range: %d", p)
		}
		if seen[p] {
			return nil, fmt.Errorf("duplicate port %d", p)
		}
		seen[p] = true
		ports = append(ports, p)
	}
	// Note: we do NOT sort ports here. The user's literal order is preserved
	// in display_scope (cmd.Scope is the source of truth for display).
	// But for the canonical per-port rows we iterate in given order.

	if host != "-" {
		if !isValidHostname(host) && net.ParseIP(host) == nil {
			return nil, fmt.Errorf("invalid host: %q", host)
		}
	}
	return &domainScope{Proto: proto, Host: host, Ports: ports}, nil
}

func canonicalScope(proto, host string, port int) string {
	return fmt.Sprintf("%s://%s:%d", proto, host, port)
}

type originEndpoint struct {
	Proto string
	Host  string
	Port  int
}

var originRE = regexp.MustCompile(`^(https?)://([a-zA-Z0-9\-\.]+):(\d+)$`)

func parseOriginEndpoint(s string) (*originEndpoint, error) {
	m := originRE.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("origin must be <proto>://<host>:<port>, got %q", s)
	}
	p, err := strconv.Atoi(m[3])
	if err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("invalid port: %s", m[3])
	}
	if !isValidHostname(m[2]) && net.ParseIP(m[2]) == nil {
		return nil, fmt.Errorf("invalid host: %q", m[2])
	}
	return &originEndpoint{Proto: m[1], Host: m[2], Port: p}, nil
}

func isValidHostname(h string) bool {
	if h == "" {
		return false
	}
	for _, r := range h {
		if !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// silence unused
var _ = sort.Ints
var _ = db.CurrentVersion
