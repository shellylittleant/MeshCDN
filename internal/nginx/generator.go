// Package nginx — config generator (UPDATED for step 6).
//
// New: BuildPoliciesFromDB attaches a DomainPolicy to each server entry,
// which the template renders into precedence-ordered nginx locations.
package nginx

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/example/meshcdn/internal/cert"
)

const DefaultDir = "/etc/meshcdn/runtime/nginx"

// generateMu serialises Generate across the whole process.
//
// Every caller (command execution, snapshot replay, boot) targets the same
// OutputDir, and Generate necessarily destroys and rebuilds it. Two concurrent
// runs interleave destructively: one walks the tree removing files while the
// other creates them, so the removal fails with ENOTEMPTY *after* it has
// already deleted nginx.conf — leaving the node with no config at all and
// every subsequent `nginx -t` failing.
//
// That is not hypothetical. On a 28-node cluster a single heartbeat round in
// which the node is behind fires one pull per peer ahead, each ending in a
// Generate; with ~300 servers to render the window is wide enough that the
// collision is close to certain.
var generateMu sync.Mutex

type Generator struct {
	OutputDir     string
	CertStore     *cert.Store
	NodeIP        string
	WelcomeRoot   string
	ChallengeRoot string

	// stageDir, when set, is where files are actually written. OutputDir stays
	// the path baked into the rendered config (the `include` directive), so the
	// staged tree is already correct for its final location.
	stageDir string
}

// writeRoot is the directory render* functions write into.
func (g *Generator) writeRoot() string {
	if g.stageDir != "" {
		return g.stageDir
	}
	return g.OutputDir
}

func New(certStore *cert.Store, nodeIP string) *Generator {
	return &Generator{
		OutputDir:     DefaultDir,
		CertStore:     certStore,
		NodeIP:        nodeIP,
		WelcomeRoot:   "/etc/meshcdn/runtime/welcome",
		ChallengeRoot: "/etc/meshcdn/runtime/challenges",
	}
}

func (g *Generator) Generate(ctx context.Context, db *sql.DB) error {
	generateMu.Lock()
	defer generateMu.Unlock()

	state, err := g.collectState(ctx, db)
	if err != nil {
		return fmt.Errorf("collect state: %w", err)
	}
	g.resolveCerts(state)

	// Step 6: build policies from bindings
	policies, err := BuildPoliciesFromDB(ctx, db)
	if err != nil {
		return fmt.Errorf("build policies: %w", err)
	}

	// Render into a sibling staging directory and swap it in only once every
	// file has been written. The old behaviour — wipe OutputDir, then spend
	// hundreds of file writes rebuilding it — meant any failure part-way
	// through (and any concurrent run) left the node with a half-built config
	// and nginx unable to start. A failed Generate must leave the previously
	// working config untouched.
	//
	// The staging dir is a sibling so the swap is a rename within one
	// filesystem.
	g.stageDir = g.OutputDir + ".staging"
	defer func() { g.stageDir = "" }()

	if err := os.RemoveAll(g.stageDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear staging dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(g.stageDir, "servers"), 0755); err != nil {
		return fmt.Errorf("mkdir staging: %w", err)
	}
	defer os.RemoveAll(g.stageDir) // no-op once the swap has renamed it away

	challengeFullDir := filepath.Join(g.ChallengeRoot, ".well-known", "acme-challenge")
	if err := os.MkdirAll(challengeFullDir, 0755); err != nil {
		return fmt.Errorf("mkdir challenge: %w", err)
	}

	if err := g.renderMain(state); err != nil {
		return fmt.Errorf("render main: %w", err)
	}

	portsWithCatchAll := make(map[int]bool)
	for _, s := range state.servers {
		policy := policies[s.Scope]
		if policy == nil {
			policy = &DomainPolicy{Scope: s.Scope}
		}
		if err := g.renderServer(s, state, policy); err != nil {
			return fmt.Errorf("render server %s: %w", s.Scope, err)
		}
		if s.Host == "-" {
			portsWithCatchAll[s.Port] = true
		}
	}

	for port, proto := range state.portProtocols {
		if portsWithCatchAll[port] {
			continue
		}
		if err := g.renderDefault(port, proto, state); err != nil {
			return fmt.Errorf("render default :%d: %w", port, err)
		}
	}

	return g.swapInStaged()
}

// swapInStaged publishes the freshly-rendered staging tree into OutputDir by
// renaming file by file, then deleting whatever the new render did not produce.
//
// The obvious implementation — rename OutputDir aside, rename staging into
// place — is not usable here. rename(2) refuses to replace a non-empty
// directory, so it takes two renames, and between them OutputDir does not
// exist. Any `nginx -t` or reload landing in that window fails exactly the way
// this whole change exists to prevent. Renaming individual files keeps the
// directory in place throughout: every path is either its old contents or its
// new contents, never absent.
func (g *Generator) swapInStaged() error {
	stagedServers := filepath.Join(g.stageDir, "servers")
	liveServers := filepath.Join(g.OutputDir, "servers")

	if err := os.MkdirAll(liveServers, 0755); err != nil {
		return fmt.Errorf("ensure output dir: %w", err)
	}

	entries, err := os.ReadDir(stagedServers)
	if err != nil {
		return fmt.Errorf("read staged servers: %w", err)
	}

	published := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if err := os.Rename(filepath.Join(stagedServers, name), filepath.Join(liveServers, name)); err != nil {
			return fmt.Errorf("publish %s: %w", name, err)
		}
		published[name] = true
	}

	// nginx.conf last: until it points at the new server set, the old one is
	// still the coherent view.
	if err := os.Rename(
		filepath.Join(g.stageDir, "nginx.conf"),
		filepath.Join(g.OutputDir, "nginx.conf"),
	); err != nil {
		return fmt.Errorf("publish nginx.conf: %w", err)
	}

	// Drop server confs this render did not produce (deleted domains).
	live, err := os.ReadDir(liveServers)
	if err != nil {
		return fmt.Errorf("read live servers: %w", err)
	}
	for _, e := range live {
		if e.IsDir() || published[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(liveServers, e.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale %s: %w", e.Name(), err)
		}
	}

	return nil
}

type generatorState struct {
	servers       []serverEntry
	portProtocols map[int]string
	allCerts      []cert.CertMeta
	selfSignedFP  string
	now           time.Time
}

type serverEntry struct {
	Scope   string
	Proto   string
	Host    string
	Port    int
	Origin  string
	CertCRT string
	CertKey string
	CertFP  string
}

func (g *Generator) collectState(ctx context.Context, db *sql.DB) (*generatorState, error) {
	st := &generatorState{
		portProtocols: make(map[int]string),
		now:           time.Now().UTC(),
	}

	if err := func() error {
		rows, err := db.QueryContext(ctx, `SELECT scope, origin FROM domains`)
		if err != nil {
			return fmt.Errorf("query domains: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var scope, origin string
			if err := rows.Scan(&scope, &origin); err != nil {
				return err
			}
			entry, err := parseServerScope(scope, origin)
			if err != nil {
				continue
			}
			st.servers = append(st.servers, *entry)
		}
		return rows.Err()
	}(); err != nil {
		return nil, err
	}

	if err := func() error {
		rows, err := db.QueryContext(ctx, `SELECT port, protocol FROM port_protocols`)
		if err != nil {
			return fmt.Errorf("query port_protocols: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var port int
			var proto string
			if err := rows.Scan(&port, &proto); err != nil {
				return err
			}
			st.portProtocols[port] = proto
		}
		return rows.Err()
	}(); err != nil {
		return nil, err
	}

	if len(st.portProtocols) == 0 {
		for _, s := range st.servers {
			st.portProtocols[s.Port] = s.Proto
		}
	}

	all, err := g.CertStore.All()
	if err != nil {
		return nil, fmt.Errorf("load certs: %w", err)
	}
	st.allCerts = all

	for _, c := range all {
		if c.Source != cert.SourceSelf {
			continue
		}
		if c.Subject == g.NodeIP {
			st.selfSignedFP = c.FingerprintPrefix
			break
		}
		for _, san := range c.SAN {
			if san == g.NodeIP {
				st.selfSignedFP = c.FingerprintPrefix
				break
			}
		}
		if st.selfSignedFP != "" {
			break
		}
	}

	sort.Slice(st.servers, func(i, j int) bool {
		if st.servers[i].Port != st.servers[j].Port {
			return st.servers[i].Port < st.servers[j].Port
		}
		return st.servers[i].Host < st.servers[j].Host
	})

	return st, nil
}

func (g *Generator) resolveCerts(st *generatorState) {
	for i := range st.servers {
		s := &st.servers[i]
		if s.Proto != "https" {
			continue
		}
		endpoint := cert.Endpoint(s.Host)
		if s.Host == "-" {
			endpoint = cert.Endpoint(g.NodeIP)
		}
		selected := cert.SelectFor(endpoint, st.allCerts, st.now)
		if selected == nil {
			if st.selfSignedFP != "" {
				crt, key := g.CertStore.Paths(st.selfSignedFP)
				s.CertCRT = crt
				s.CertKey = key
				s.CertFP = st.selfSignedFP
			}
			continue
		}
		crt, key := g.CertStore.Paths(selected.FingerprintPrefix)
		s.CertCRT = crt
		s.CertKey = key
		s.CertFP = selected.FingerprintPrefix
	}
}

func parseServerScope(scope, origin string) (*serverEntry, error) {
	idx := strings.Index(scope, "://")
	if idx < 0 {
		return nil, fmt.Errorf("invalid scope %q", scope)
	}
	proto := scope[:idx]
	rest := scope[idx+3:]
	colon := strings.LastIndex(rest, ":")
	if colon < 0 {
		return nil, fmt.Errorf("scope missing port: %q", scope)
	}
	host := rest[:colon]
	portStr := rest[colon+1:]
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port: %v", err)
	}
	return &serverEntry{
		Scope:  scope,
		Proto:  proto,
		Host:   host,
		Port:   port,
		Origin: origin,
	}, nil
}

func (g *Generator) renderMain(_ *generatorState) error {
	t, err := template.New("main").Parse(mainConfTemplate)
	if err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(g.writeRoot(), "nginx.conf"))
	if err != nil {
		return err
	}
	defer f.Close()
	return t.Execute(f, map[string]interface{}{
		"OutputDir":     g.OutputDir,
		"WelcomeRoot":   g.WelcomeRoot,
		"ChallengeRoot": g.ChallengeRoot,
	})
}

// luaQuote renders s as a safe Lua double-quoted string literal for embedding
// in an access_by_lua_block. User-Agent regex patterns reach here as data, never
// code; combined with validateUARegex (which already rejected control chars),
// escaping backslash/quote/CR/LF makes Lua-string injection impossible.
func luaQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// renderServer now takes a DomainPolicy.
func (g *Generator) renderServer(s serverEntry, st *generatorState, policy *DomainPolicy) error {
	t, err := template.New("server").Funcs(template.FuncMap{
		"luaQuote": luaQuote,
	}).Parse(serverConfTemplate)
	if err != nil {
		return err
	}
	filename := fmt.Sprintf("domain-%s-%d.conf", sanitizeFilename(s.Host), s.Port)
	f, err := os.Create(filepath.Join(g.writeRoot(), "servers", filename))
	if err != nil {
		return err
	}
	defer f.Close()

	return t.Execute(f, map[string]interface{}{
		"Server":          s,
		"WelcomeRoot":     g.WelcomeRoot,
		"ChallengeRoot":   g.ChallengeRoot,
		"OriginIsWelcome": s.Origin == "-",
		"OriginRaw":       s.Origin,
		"Policy":          policy,
	})
}

func (g *Generator) renderDefault(port int, proto string, st *generatorState) error {
	t, err := template.New("default").Parse(defaultConfTemplate)
	if err != nil {
		return err
	}
	filename := fmt.Sprintf("default-%d.conf", port)
	f, err := os.Create(filepath.Join(g.writeRoot(), "servers", filename))
	if err != nil {
		return err
	}
	defer f.Close()

	certCRT, certKey := "", ""
	if proto == "https" && st.selfSignedFP != "" {
		certCRT, certKey = g.CertStore.Paths(st.selfSignedFP)
	}

	return t.Execute(f, map[string]interface{}{
		"Port":          port,
		"Proto":         proto,
		"CertCRT":       certCRT,
		"CertKey":       certKey,
		"WelcomeRoot":   g.WelcomeRoot,
		"ChallengeRoot": g.ChallengeRoot,
	})
}

func sanitizeFilename(s string) string {
	if s == "-" {
		return "any"
	}
	out := strings.Builder{}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.' || r == '-':
			out.WriteRune(r)
		default:
			out.WriteRune('_')
		}
	}
	return out.String()
}

func ValidateNetwork(ip string) bool {
	return net.ParseIP(ip) != nil
}
