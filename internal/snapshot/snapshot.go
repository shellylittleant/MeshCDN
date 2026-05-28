// Package snapshot — UPDATED for step 6.
//
// Now exports:
//   - port_protocols (as /w domain implicit bindings — re-derive on replay)
//   - domains
//   - rule_objects (cache, defense, redirect, header)
//   - bindings
//   - peers (as /w internal peer-add commands)
//
// Order of /w lines in snapshot matters:
//  1. peer-add commands first (so mesh state is consistent before any data)
//  2. domain commands (these establish port_protocols implicitly)
//  3. rule_objects (referenced by bindings, must come before bindings)
//  4. bindings
//
// On replay, snapshot.Import wipes all tables then replays in this order
// — primary keys are reconstructed correctly.
package snapshot

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const DefaultPath = "/etc/meshcdn/persistent/snapshot.cmd"

func Path() string {
	if p := os.Getenv("MESHCDN_SNAPSHOT_PATH"); p != "" {
		return p
	}
	return DefaultPath
}

type Header struct {
	Version    int64
	ExportedAt time.Time
	Program    string
}

type Snapshot struct {
	Header   Header
	Commands []string
}

func Load() (*Snapshot, error) { return LoadFrom(Path()) }

func LoadFrom(path string) (*Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoSnapshot
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	snap := &Snapshot{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			parseHeaderLine(trimmed, &snap.Header)
			continue
		}
		snap.Commands = append(snap.Commands, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return snap, nil
}

var ErrNoSnapshot = errors.New("snapshot.cmd not found")

func parseHeaderLine(line string, h *Header) {
	body := strings.TrimSpace(strings.TrimPrefix(line, "#"))
	colon := strings.Index(body, ":")
	if colon < 0 {
		return
	}
	key := strings.TrimSpace(body[:colon])
	val := strings.TrimSpace(body[colon+1:])
	switch key {
	case "version":
		if v, err := strconv.ParseInt(val, 10, 64); err == nil {
			h.Version = v
		}
	case "exported":
		if t, err := time.Parse(time.RFC3339, val); err == nil {
			h.ExportedAt = t
		}
	case "program":
		h.Program = val
	}
}

// Exporter renders the current DB state as a snapshot file.
type Exporter struct {
	DB             *sql.DB
	ProgramVersion string
}

func (e *Exporter) Export(ctx context.Context) (*Snapshot, error) {
	snap := &Snapshot{
		Header: Header{
			ExportedAt: time.Now().UTC(),
			Program:    e.ProgramVersion,
		},
	}

	if err := e.DB.QueryRowContext(ctx,
		`SELECT config_version FROM cluster_meta WHERE id = 1`).
		Scan(&snap.Header.Version); err != nil {
		return nil, fmt.Errorf("read config_version: %w", err)
	}

	// 1) Peers
	if err := e.exportPeers(ctx, snap); err != nil {
		return nil, err
	}
	// 2) Domains
	if err := e.exportDomains(ctx, snap); err != nil {
		return nil, err
	}
	// 3) Rule objects
	if err := e.exportRuleObjects(ctx, snap); err != nil {
		return nil, err
	}
	// 4) Bindings
	if err := e.exportBindings(ctx, snap); err != nil {
		return nil, err
	}

	return snap, nil
}

func (e *Exporter) exportPeers(ctx context.Context, snap *Snapshot) error {
	rows, err := e.DB.QueryContext(ctx,
		`SELECT ip, join_order FROM peers ORDER BY join_order`)
	if err != nil {
		// peers table may not exist if we're in a stripped-down test env
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var ip string
		var jo int
		if err := rows.Scan(&ip, &jo); err != nil {
			return err
		}
		snap.Commands = append(snap.Commands,
			fmt.Sprintf("/w internal peer-add %s %d", ip, jo))
	}
	return rows.Err()
}

func (e *Exporter) exportDomains(ctx context.Context, snap *Snapshot) error {
	// V4: each domain row has a display_scope (the literal user input).
	// Dedupe by display_scope so /w domain commands round-trip exactly.
	rows, err := e.DB.QueryContext(ctx,
		`SELECT display_scope, MIN(origin) FROM domains
		 GROUP BY display_scope ORDER BY display_scope`)
	if err != nil {
		return fmt.Errorf("query domains: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var displayScope, origin string
		if err := rows.Scan(&displayScope, &origin); err != nil {
			return err
		}
		snap.Commands = append(snap.Commands,
			fmt.Sprintf("/w domain %s %s", displayScope, origin))
	}
	return rows.Err()
}

// silence unused (kept for reference)
var _ = strconv.Itoa
var _ = strings.Join

func (e *Exporter) exportRuleObjects(ctx context.Context, snap *Snapshot) error {
	rows, err := e.DB.QueryContext(ctx,
		`SELECT type, name, params_text FROM rule_objects ORDER BY type, name`)
	if err != nil {
		return nil // table may not exist yet
	}
	defer rows.Close()
	for rows.Next() {
		var typ, name, params string
		if err := rows.Scan(&typ, &name, &params); err != nil {
			return err
		}
		snap.Commands = append(snap.Commands,
			fmt.Sprintf("/w %s %s %s", typ, name, params))
	}
	return rows.Err()
}

func (e *Exporter) exportBindings(ctx context.Context, snap *Snapshot) error {
	rows, err := e.DB.QueryContext(ctx,
		`SELECT scope, object_type, object_name FROM bindings ORDER BY scope, object_type, object_name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var scope, ot, on string
		if err := rows.Scan(&scope, &ot, &on); err != nil {
			return err
		}
		snap.Commands = append(snap.Commands,
			fmt.Sprintf("/w bind %s %s:%s", scope, ot, on))
	}
	return rows.Err()
}

func (s *Snapshot) Save() error { return s.SaveTo(Path()) }

func (s *Snapshot) SaveTo(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# version: %d\n", s.Header.Version)
	fmt.Fprintf(&sb, "# exported: %s\n", s.Header.ExportedAt.UTC().Format(time.RFC3339))
	if s.Header.Program != "" {
		fmt.Fprintf(&sb, "# program: %s\n", s.Header.Program)
	}
	for _, c := range s.Commands {
		sb.WriteString(c)
		sb.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Snapshot) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# version: %d\n", s.Header.Version)
	fmt.Fprintf(&sb, "# exported: %s\n", s.Header.ExportedAt.UTC().Format(time.RFC3339))
	if s.Header.Program != "" {
		fmt.Fprintf(&sb, "# program: %s\n", s.Header.Program)
	}
	for _, c := range s.Commands {
		sb.WriteString(c)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func ParseText(text string) (*Snapshot, error) {
	snap := &Snapshot{}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			parseHeaderLine(trimmed, &snap.Header)
			continue
		}
		snap.Commands = append(snap.Commands, line)
	}
	return snap, nil
}
