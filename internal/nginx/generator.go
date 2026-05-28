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
	"text/template"
	"time"

	"github.com/example/meshcdn/internal/cert"
)

const DefaultDir = "/etc/meshcdn/runtime/nginx"

type Generator struct {
	OutputDir     string
	CertStore     *cert.Store
	NodeIP        string
	WelcomeRoot   string
	ChallengeRoot string
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

	if err := os.RemoveAll(g.OutputDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove old config: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(g.OutputDir, "servers"), 0755); err != nil {
		return fmt.Errorf("mkdir output: %w", err)
	}

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
	f, err := os.Create(filepath.Join(g.OutputDir, "nginx.conf"))
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

// renderServer now takes a DomainPolicy.
func (g *Generator) renderServer(s serverEntry, st *generatorState, policy *DomainPolicy) error {
	t, err := template.New("server").Parse(serverConfTemplate)
	if err != nil {
		return err
	}
	filename := fmt.Sprintf("domain-%s-%d.conf", sanitizeFilename(s.Host), s.Port)
	f, err := os.Create(filepath.Join(g.OutputDir, "servers", filename))
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
	f, err := os.Create(filepath.Join(g.OutputDir, "servers", filename))
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
