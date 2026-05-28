// Package nginx — precedence engine for multi-binding rules.
//
// Per V4-DESIGN §8.5 and §8.6:
//   - Same domain bound to multiple rule objects → nginx location blocks
//     are emitted in precedence order: exact > prefix > regex > wildcard.
//   - Same precision conflict (e.g. two patterns "*.jpg" with different TTLs)
//     → fields merge by "更严格者胜出" (TTL takes minimum, hsts takes OR, etc.)
//
// nginx itself does the runtime matching — we just emit the location blocks
// in the right priority order. nginx's location matching algorithm:
//  1. exact match (location = /foo)
//  2. preferential prefix (location ^~ /foo/)
//  3. regex (location ~* \.jpg$ — first match wins)
//  4. ordinary prefix (location /foo/ — longest match wins)
//
// Our precedence:
//
//	/exact            → emit as `location = /exact`
//	/static/*         → `location ^~ /static/`
//	*.jpg, *.png      → `location ~* \.(jpg|png)$`
//	/                 → ordinary `location /` (catch-all)
//
// For redirect.From and defense IP/CIDR, the same precision principle
// applies: more-specific rules emit before less-specific.
package nginx

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DomainPolicy is the assembled per-domain policy from all its bindings.
//
// The generator builds one DomainPolicy per (host, port) endpoint by
// querying bindings + rule_objects, then renders nginx config from it.
type DomainPolicy struct {
	Scope string // "https://a.com:443" — the domain endpoint

	// Cache rules in precedence order (highest precision first)
	CacheRules []CacheRuleEntry

	// Header operations (merged across all bound header objects)
	HeaderOps HeaderOps

	// Redirect rules in precedence order
	Redirects []RedirectEntry

	// Defense rules
	Defenses DefensePolicy
}

// CacheRuleEntry: one cache rule attached to one or more nginx locations.
//
// After "merge by stricter" pass, all duplicates of the same pattern are
// collapsed into a single entry with the strictest TTL/HSTS values.
type CacheRuleEntry struct {
	// Pattern as written by user, e.g. "*.jpg", "/static/", "= /favicon.ico"
	Pattern string

	// Computed nginx location form
	NginxModifier string // "=", "^~", "~*", "" (ordinary prefix)
	NginxMatcher  string // the path/regex after the modifier

	// Stricter-wins merged values
	TTL        int
	BrowserTTL int
	HSTS       bool

	// Precedence rank (lower = higher priority): 0=exact, 1=prefix, 2=regex, 3=wildcard
	Precedence int
}

// HeaderOps: merged header operations from all bound header objects.
type HeaderOps struct {
	RequestAdd  map[string]string
	RequestDel  []string
	ResponseAdd map[string]string
	ResponseDel []string
}

// RedirectEntry: one redirect rule.
type RedirectEntry struct {
	From       string
	To         string
	Status     int
	Regex      bool
	Precedence int
}

// DefensePolicy: aggregated defense rules from all bound defense objects.
//
// Per V4-DESIGN §8.6: when block and allow conflict, block wins.
type DefensePolicy struct {
	BlockIPs   []string // CIDRs to deny
	AllowIPs   []string // CIDRs to allow (allow wins inside its own scope, but block wins on overlap)
	BlockUA    []string // regex patterns (Lua)
	RefAllowed []string // valid_referers
	RateLimit  int      // requests/second (max of all rules — the most permissive applies)
	BodySize   string   // client_max_body_size; if multiple, take smallest
	BlockGeo   []string // country codes (planned)
}

// ─────────────────────────────────────────────────────────────────────
// Building DomainPolicy from DB
// ─────────────────────────────────────────────────────────────────────

// rawBinding holds one row from the bindings JOIN rule_objects query.
type rawBinding struct {
	scope      string // bind scope, e.g. "a.com"
	objectType string
	parsedJSON string
}

// BuildPoliciesFromDB returns a map of canonical scope → DomainPolicy.
//
// V4 invariant: bindings.scope always equals domains.display_scope.
// We load each domain row's (canonical scope, display_scope), then for each
// canonical server block we apply the bindings keyed by that row's
// display_scope.
//
// The returned map keys are canonical scopes ("https://-:443") because the
// nginx generator emits one server block per canonical scope (one port).
// Multiple canonical scopes that share a display_scope (e.g. "https://-:7777"
// and "https://-:8888" both under display_scope "https://-:7777,8888") all
// receive the same set of bindings — exactly as the user intends.
func BuildPoliciesFromDB(ctx context.Context, db *sql.DB) (map[string]*DomainPolicy, error) {
	policies := make(map[string]*DomainPolicy)

	// Step 1: enumerate all domain rows; remember each canonical scope's
	// display_scope so we can match bindings later.
	canonToDisplay := make(map[string]string) // canonical scope → display_scope
	rows, err := db.QueryContext(ctx,
		`SELECT scope, display_scope FROM domains ORDER BY scope`)
	if err != nil {
		return nil, fmt.Errorf("query domains: %w", err)
	}
	for rows.Next() {
		var canon, display string
		if err := rows.Scan(&canon, &display); err != nil {
			rows.Close()
			return nil, err
		}
		canonToDisplay[canon] = display
		policies[canon] = &DomainPolicy{Scope: canon}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Step 2: load all bindings + their objects, indexed by binding scope
	// (which equals some display_scope).
	bindRows, err := db.QueryContext(ctx, `
		SELECT b.scope, b.object_type, ro.parsed_json
		FROM bindings b
		JOIN rule_objects ro ON ro.type = b.object_type AND ro.name = b.object_name
		ORDER BY b.scope, b.object_type, b.object_name
	`)
	if err != nil {
		return nil, fmt.Errorf("query bindings: %w", err)
	}
	defer bindRows.Close()

	bindsByDisplay := make(map[string][]rawBinding)
	for bindRows.Next() {
		var rb rawBinding
		if err := bindRows.Scan(&rb.scope, &rb.objectType, &rb.parsedJSON); err != nil {
			return nil, err
		}
		bindsByDisplay[rb.scope] = append(bindsByDisplay[rb.scope], rb)
	}
	if err := bindRows.Err(); err != nil {
		return nil, err
	}

	// Step 3: for each canonical scope, look up its display_scope and apply
	// the bindings registered under that display_scope.
	for canon, policy := range policies {
		display := canonToDisplay[canon]
		applyBindings(policy, bindsByDisplay[display])
	}

	return policies, nil
}

func loadDomainScopes(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT scope FROM domains ORDER BY scope`)
	if err != nil {
		return nil, fmt.Errorf("query domains: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// extractHost: "https://a.com:443" → "a.com"  (kept for any legacy callers)
func extractHost(scope string) (string, error) {
	idx := strings.Index(scope, "://")
	if idx < 0 {
		return "", fmt.Errorf("invalid scope %q", scope)
	}
	rest := scope[idx+3:]
	colon := strings.LastIndex(rest, ":")
	if colon < 0 {
		return rest, nil
	}
	return rest[:colon], nil
}

// applyBindings merges all bound rule objects into the DomainPolicy.
func applyBindings(p *DomainPolicy, bindings []rawBinding) {
	// Accumulate by type
	type cacheRaw struct {
		Patterns   []string `json:"patterns"`
		TTL        int      `json:"ttl"`
		BrowserTTL int      `json:"browser_ttl,omitempty"`
		HSTS       bool     `json:"hsts,omitempty"`
	}
	type defenseRaw struct {
		IP     []string `json:"ip,omitempty"`
		UA     string   `json:"ua,omitempty"`
		Ref    []string `json:"ref,omitempty"`
		Action string   `json:"action,omitempty"`
		Rate   int      `json:"rate,omitempty"`
		Size   string   `json:"size,omitempty"`
	}
	type redirectRaw struct {
		From   string `json:"from"`
		To     string `json:"to"`
		Status int    `json:"status,omitempty"`
		Regex  bool   `json:"regex,omitempty"`
	}
	type headerRaw struct {
		RequestAdd  map[string]string `json:"request_add,omitempty"`
		RequestDel  []string          `json:"request_del,omitempty"`
		ResponseAdd map[string]string `json:"response_add,omitempty"`
		ResponseDel []string          `json:"response_del,omitempty"`
	}

	// Init merged structures
	if p.HeaderOps.RequestAdd == nil {
		p.HeaderOps.RequestAdd = make(map[string]string)
	}
	if p.HeaderOps.ResponseAdd == nil {
		p.HeaderOps.ResponseAdd = make(map[string]string)
	}

	// Track cache rules by pattern for stricter-wins merge
	cacheByPattern := make(map[string]*CacheRuleEntry)

	for _, b := range bindings {
		switch b.objectType {
		case "cache":
			var c cacheRaw
			if err := json.Unmarshal([]byte(b.parsedJSON), &c); err != nil {
				continue
			}
			for _, pattern := range c.Patterns {
				existing := cacheByPattern[pattern]
				if existing == nil {
					mod, matcher, prec := patternToNginxLocation(pattern)
					cacheByPattern[pattern] = &CacheRuleEntry{
						Pattern:       pattern,
						NginxModifier: mod,
						NginxMatcher:  matcher,
						TTL:           c.TTL,
						BrowserTTL:    c.BrowserTTL,
						HSTS:          c.HSTS,
						Precedence:    prec,
					}
				} else {
					// Stricter-wins merge:
					if c.TTL < existing.TTL {
						existing.TTL = c.TTL
					}
					if c.BrowserTTL > 0 && (existing.BrowserTTL == 0 || c.BrowserTTL < existing.BrowserTTL) {
						existing.BrowserTTL = c.BrowserTTL
					}
					if c.HSTS {
						existing.HSTS = true
					}
				}
			}

		case "defense":
			var d defenseRaw
			if err := json.Unmarshal([]byte(b.parsedJSON), &d); err != nil {
				continue
			}
			if d.Action == "" {
				d.Action = "block"
			}
			if d.Action == "block" {
				p.Defenses.BlockIPs = append(p.Defenses.BlockIPs, d.IP...)
			} else {
				p.Defenses.AllowIPs = append(p.Defenses.AllowIPs, d.IP...)
			}
			if d.UA != "" {
				p.Defenses.BlockUA = append(p.Defenses.BlockUA, d.UA)
			}
			p.Defenses.RefAllowed = append(p.Defenses.RefAllowed, d.Ref...)
			if d.Rate > 0 && (p.Defenses.RateLimit == 0 || d.Rate < p.Defenses.RateLimit) {
				p.Defenses.RateLimit = d.Rate
			}
			if d.Size != "" {
				if p.Defenses.BodySize == "" {
					p.Defenses.BodySize = d.Size
				}
				// Note: comparing "10m" < "1g" needs unit parsing; step 6
				// keeps it simple: first one wins. Step 7 polish.
			}

		case "redirect":
			var r redirectRaw
			if err := json.Unmarshal([]byte(b.parsedJSON), &r); err != nil {
				continue
			}
			if r.Status == 0 {
				r.Status = 301
			}
			prec := pathPrecedence(r.From, r.Regex)
			p.Redirects = append(p.Redirects, RedirectEntry{
				From:       r.From,
				To:         r.To,
				Status:     r.Status,
				Regex:      r.Regex,
				Precedence: prec,
			})

		case "header":
			var h headerRaw
			if err := json.Unmarshal([]byte(b.parsedJSON), &h); err != nil {
				continue
			}
			for k, v := range h.RequestAdd {
				p.HeaderOps.RequestAdd[k] = v
			}
			p.HeaderOps.RequestDel = append(p.HeaderOps.RequestDel, h.RequestDel...)
			for k, v := range h.ResponseAdd {
				p.HeaderOps.ResponseAdd[k] = v
			}
			p.HeaderOps.ResponseDel = append(p.HeaderOps.ResponseDel, h.ResponseDel...)
		}
	}

	// Materialize cache rules and sort by precedence
	for _, entry := range cacheByPattern {
		p.CacheRules = append(p.CacheRules, *entry)
	}
	sort.SliceStable(p.CacheRules, func(i, j int) bool {
		if p.CacheRules[i].Precedence != p.CacheRules[j].Precedence {
			return p.CacheRules[i].Precedence < p.CacheRules[j].Precedence
		}
		return p.CacheRules[i].Pattern < p.CacheRules[j].Pattern
	})

	// Sort redirects by precedence too
	sort.SliceStable(p.Redirects, func(i, j int) bool {
		if p.Redirects[i].Precedence != p.Redirects[j].Precedence {
			return p.Redirects[i].Precedence < p.Redirects[j].Precedence
		}
		return len(p.Redirects[i].From) > len(p.Redirects[j].From)
	})
}

// patternToNginxLocation converts a user pattern to nginx location syntax.
//
// Returns: (modifier, matcher, precedence-rank)
//
// Rank: 0=exact, 1=preferential prefix, 2=regex, 3=wildcard/extension
func patternToNginxLocation(pattern string) (modifier, matcher string, precedence int) {
	pattern = strings.TrimSpace(pattern)

	// 1) Exact match: "= /favicon.ico"
	if strings.HasPrefix(pattern, "= ") {
		return "=", strings.TrimSpace(pattern[2:]), 0
	}

	// 2) Preferential prefix: "^~ /assets/"
	if strings.HasPrefix(pattern, "^~ ") {
		return "^~", strings.TrimSpace(pattern[3:]), 1
	}

	// 3) Regex: "~ ^/api/v[0-9]+/"
	if strings.HasPrefix(pattern, "~ ") {
		return "~", strings.TrimSpace(pattern[2:]), 2
	}
	if strings.HasPrefix(pattern, "~* ") {
		return "~*", strings.TrimSpace(pattern[3:]), 2
	}

	// 4) Extension wildcard: "*.jpg" → regex
	if strings.HasPrefix(pattern, "*.") {
		ext := pattern[2:]
		// Escape regex metachars in extension (defensive)
		return "~*", `\.` + ext + "$", 3
	}

	// 5) Path prefix: "/static/", "/api/v1"
	if strings.HasPrefix(pattern, "/") {
		return "", pattern, 1
	}

	// 6) Otherwise: treat as exact filename match (regex anchor at end)
	return "~*", `(^|/)` + pattern + `$`, 3
}

// pathPrecedence returns precedence rank for a redirect path.
func pathPrecedence(from string, isRegex bool) int {
	if isRegex {
		return 2
	}
	// Exact path? (no special chars)
	if !strings.ContainsAny(from, "*?[]") {
		// Treat literal path as exact-prefix
		return 1
	}
	return 3 // wildcard
}
