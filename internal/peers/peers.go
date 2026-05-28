// Package peers owns persistent/peers.json — the cluster's node membership list.
//
// Per V4-DESIGN §A.1 and §5.2:
//   - This is one of the four files preserved across upgrades.
//   - peers.json mirrors the in-DB peers table; the json file is the
//     persistent truth, the DB is a query-friendly cache.
//   - Membership changes (peer joining/leaving) propagate via the config
//     stream — i.e. handled as /internal/peer-add commands that bump
//     config_version. peers.json is updated as a side-effect of those
//     commands committing.
//
// File format:
//
//	{
//	  "peers": [
//	    { "ip": "1.2.3.4", "join_order": 1 },
//	    { "ip": "5.6.7.8", "join_order": 2 }
//	  ]
//	}
//
// Concurrency: Manager methods are safe for concurrent use via internal mutex.
package peers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const DefaultPath = "/etc/meshcdn/persistent/peers.json"

// Path returns the effective peers.json path (env-overridable).
func Path() string {
	if p := os.Getenv("MESHCDN_PEERS_PATH"); p != "" {
		return p
	}
	return DefaultPath
}

// Peer represents one cluster member.
type Peer struct {
	IP        string `json:"ip"`
	JoinOrder int    `json:"join_order"`
}

// File is the on-disk JSON layout.
type File struct {
	Peers []Peer `json:"peers"`
}

// Manager handles atomic read/modify/write of peers.json.
//
// Manager is the single point of access to peers.json from anywhere in
// the agent. Other packages (mesh, command handlers) call Manager methods
// rather than touching the file directly.
type Manager struct {
	path string
	mu   sync.Mutex
}

// NewManager opens (or creates) a peers.json manager at the given path.
// If the file doesn't exist, an empty list is created.
func NewManager(path string) (*Manager, error) {
	m := &Manager{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("mkdir for peers.json: %w", err)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := m.write(File{Peers: []Peer{}}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return m, nil
}

// All returns the current peer list (sorted by join_order).
func (m *Manager) All() ([]Peer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.read()
	if err != nil {
		return nil, err
	}
	out := make([]Peer, len(f.Peers))
	copy(out, f.Peers)
	sort.Slice(out, func(i, j int) bool { return out[i].JoinOrder < out[j].JoinOrder })
	return out, nil
}

// Has reports whether ip is in the peer list.
func (m *Manager) Has(ip string) (bool, error) {
	all, err := m.All()
	if err != nil {
		return false, err
	}
	for _, p := range all {
		if p.IP == ip {
			return true, nil
		}
	}
	return false, nil
}

// Add inserts a new peer with auto-assigned join_order.
// If the IP already exists, returns its existing entry without modification.
//
// Returns the resulting Peer (either newly added or existing).
func (m *Manager) Add(ip string) (*Peer, error) {
	if net.ParseIP(ip) == nil {
		return nil, fmt.Errorf("invalid IP: %q", ip)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.read()
	if err != nil {
		return nil, err
	}
	// Already exists?
	for _, p := range f.Peers {
		if p.IP == ip {
			pp := p
			return &pp, nil
		}
	}
	// Compute next join_order = max + 1
	maxOrder := 0
	for _, p := range f.Peers {
		if p.JoinOrder > maxOrder {
			maxOrder = p.JoinOrder
		}
	}
	newPeer := Peer{IP: ip, JoinOrder: maxOrder + 1}
	f.Peers = append(f.Peers, newPeer)
	if err := m.write(f); err != nil {
		return nil, err
	}
	return &newPeer, nil
}

// Remove deletes a peer by IP. No-op (returns nil) if not present.
func (m *Manager) Remove(ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.read()
	if err != nil {
		return err
	}
	out := make([]Peer, 0, len(f.Peers))
	for _, p := range f.Peers {
		if p.IP != ip {
			out = append(out, p)
		}
	}
	if len(out) == len(f.Peers) {
		return nil // not present
	}
	f.Peers = out
	return m.write(f)
}

// ReplaceAll overwrites the peer list entirely. Used by snapshot import.
func (m *Manager) ReplaceAll(list []Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.write(File{Peers: list})
}

// BotNodeIP returns the IP of the peer with the smallest join_order, which
// V4 designates as the bot node by default. Per V4-DESIGN §5.3, the first
// peer to join is the default bot node; manual transfer (/v target ...) is
// supported but uses a separate mechanism (not yet implemented in step 9).
//
// Returns "" if the peer list is empty.
//
// This is the source of truth for "who handles Telegram?" — used by mesh
// alert forwarding (worker nodes look up bot IP and POST events to it).
func (m *Manager) BotNodeIP() string {
	all, err := m.All()
	if err != nil || len(all) == 0 {
		return ""
	}
	// All() already sorts by join_order ascending.
	return all[0].IP
}

// ─────────────────────────────────────────────────────────────────────
// Internal IO
// ─────────────────────────────────────────────────────────────────────

func (m *Manager) read() (File, error) {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return File{}, fmt.Errorf("read %s: %w", m.path, err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("parse %s: %w", m.path, err)
	}
	return f, nil
}

func (m *Manager) write(f File) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}
