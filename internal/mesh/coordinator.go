// Mesh coordinator — the "blood" component that keeps cluster state in sync.
//
// Per V4-DESIGN §1:
//
//	1.1 Every minute: ping each peer, exchange (config_version, program_version, peer_count)
//	1.2 After local mutation: notify-version push to all peers
//	1.3 On lag detection: pull snapshot from RTT-best peer that's at-or-ahead
//
// The Coordinator owns these three behaviors as a unit because they share
// state (peer RTT table, last-known peer versions). It runs on its own
// goroutine, started by serve.go.
package mesh

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"
)

// HeartbeatInterval is how often we ping peers.
const HeartbeatInterval = 1 * time.Minute

// PingTimeout is per-ping timeout.
const PingTimeout = 5 * time.Second

// PeerState tracks what we last saw from a peer.
type PeerState struct {
	IP             string
	JoinOrder      int
	LastSeenAt     time.Time
	LastRTT        time.Duration
	ConfigVersion  int64
	ProgramVersion string
	PeerKnownCount int

	// ConsecutiveFailures: number of pings in a row that failed.
	// After 3 (≥3 minutes silent), peer is considered offline (V4-DESIGN
	// "bot 失联=3 min" — applies to all peers, not just bot).
	ConsecutiveFailures int
}

// Coordinator runs the three blood-loop behaviors.
type Coordinator struct {
	Client *Client
	Port   int

	// Wired by serve.go:
	LocalNodeIP    string
	GetLocalState  func() (configVersion int64, programVersion string, peerCount int)
	GetPeers       func() ([]PeerInfo, error)
	OnRemoteAhead  func(peerIP string, remoteVersion int64)       // called when peer.version > local
	OnReceiveAhead func(remoteVersion int64) (int64, error)       // returns local current version
	PullAndApply   func(ctx context.Context, peerIP string) error // does snapshot pull + replay

	// Internal state
	mu     sync.RWMutex
	states map[string]*PeerState // ip → state
}

// NewCoordinator returns a Coordinator with empty state. Caller must wire up
// the function pointers before calling Run.
func NewCoordinator(client *Client, port int) *Coordinator {
	return &Coordinator{
		Client: client,
		Port:   port,
		states: make(map[string]*PeerState),
	}
}

// Run starts the heartbeat loop. Blocks until ctx is canceled.
func (c *Coordinator) Run(ctx context.Context) {
	// Run an initial heartbeat immediately so a freshly-started agent
	// learns peer state without waiting a minute.
	c.runHeartbeatPass(ctx)

	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runHeartbeatPass(ctx)
		}
	}
}

// runHeartbeatPass pings every peer in parallel.
func (c *Coordinator) runHeartbeatPass(ctx context.Context) {
	peers, err := c.GetPeers()
	if err != nil {
		log.Printf("[mesh/coord] get peers: %v", err)
		return
	}

	configV, progV, peerCount := c.GetLocalState()

	var wg sync.WaitGroup
	for _, p := range peers {
		if p.IP == c.LocalNodeIP {
			continue // don't ping ourselves
		}
		wg.Add(1)
		go func(peer PeerInfo) {
			defer wg.Done()
			c.pingOne(ctx, peer, configV, progV, peerCount)
		}(p)
	}
	wg.Wait()
}

// pingOne sends a single ping and updates state.
func (c *Coordinator) pingOne(ctx context.Context, peer PeerInfo, configV int64, progV string, peerCount int) {
	pingCtx, cancel := context.WithTimeout(ctx, PingTimeout)
	defer cancel()

	req := &PingRequest{
		NodeIP:         c.LocalNodeIP,
		ConfigVersion:  configV,
		ProgramVersion: progV,
		PeerKnownCount: peerCount,
	}

	start := time.Now()
	resp, err := c.Client.Ping(pingCtx, peer.IP, c.Port, req)
	rtt := time.Since(start)

	c.mu.Lock()
	st, ok := c.states[peer.IP]
	if !ok {
		st = &PeerState{IP: peer.IP, JoinOrder: peer.JoinOrder}
		c.states[peer.IP] = st
	}

	if err != nil {
		st.ConsecutiveFailures++
		c.mu.Unlock()
		if st.ConsecutiveFailures == 1 {
			// Log first failure, then go quiet
			log.Printf("[mesh/coord] ping %s failed: %v", peer.IP, err)
		}
		return
	}

	st.LastSeenAt = time.Now()
	st.LastRTT = rtt
	st.ConfigVersion = resp.ConfigVersion
	st.ProgramVersion = resp.ProgramVersion
	st.PeerKnownCount = resp.PeerKnownCount
	prevFailures := st.ConsecutiveFailures
	st.ConsecutiveFailures = 0
	c.mu.Unlock()

	if prevFailures >= 3 {
		log.Printf("[mesh/coord] peer %s recovered after %d failures", peer.IP, prevFailures)
	}

	// React to version difference
	if resp.ConfigVersion > configV {
		log.Printf("[mesh/coord] peer %s is ahead (v%d vs our v%d)", peer.IP, resp.ConfigVersion, configV)
		if c.OnRemoteAhead != nil {
			c.OnRemoteAhead(peer.IP, resp.ConfigVersion)
		}
	}
}

// HandleNotifyVersion processes an incoming NotifyVersion call.
// Peer says they have version V; we compare to local and pull if behind.
//
// This is the server-side handler wired into Server.OnNotifyVersion.
func (c *Coordinator) HandleNotifyVersion(req *NotifyVersionRequest) error {
	if c.OnReceiveAhead == nil {
		return errors.New("OnReceiveAhead not wired")
	}
	localV, err := c.OnReceiveAhead(req.ConfigVersion)
	if err != nil {
		return err
	}
	if req.ConfigVersion > localV {
		log.Printf("[mesh/coord] notify-version from %s: v%d (we're v%d), triggering pull",
			req.FromIP, req.ConfigVersion, localV)
		if c.OnRemoteAhead != nil {
			c.OnRemoteAhead(req.FromIP, req.ConfigVersion)
		}
	}
	return nil
}

// BestPeerToPull returns the IP of the peer with lowest RTT among peers
// at version ≥ targetVersion. Returns empty string if no eligible peer.
func (c *Coordinator) BestPeerToPull(targetVersion int64) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	type cand struct {
		ip  string
		rtt time.Duration
	}
	var candidates []cand
	for ip, st := range c.states {
		if st.ConfigVersion < targetVersion {
			continue
		}
		if st.ConsecutiveFailures > 0 {
			continue
		}
		candidates = append(candidates, cand{ip: ip, rtt: st.LastRTT})
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].rtt < candidates[j].rtt
	})
	return candidates[0].ip
}

// PeerStates returns a snapshot of the current peer state map (for diagnostics).
func (c *Coordinator) PeerStates() map[string]PeerState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]PeerState, len(c.states))
	for k, v := range c.states {
		out[k] = *v
	}
	return out
}

// PushNotifyToAll sends a notify-version to every peer in parallel.
// Best-effort: failures are logged but don't bubble up.
//
// Called by the executor after a successful batch commits a new version.
func (c *Coordinator) PushNotifyToAll(ctx context.Context, newVersion int64) {
	peers, err := c.GetPeers()
	if err != nil {
		log.Printf("[mesh/coord] PushNotifyToAll: get peers: %v", err)
		return
	}

	req := &NotifyVersionRequest{
		FromIP:        c.LocalNodeIP,
		ConfigVersion: newVersion,
	}

	var wg sync.WaitGroup
	for _, p := range peers {
		if p.IP == c.LocalNodeIP {
			continue
		}
		wg.Add(1)
		go func(peerIP string) {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := c.Client.NotifyVersion(callCtx, peerIP, c.Port, req); err != nil {
				log.Printf("[mesh/coord] notify %s: %v", peerIP, err)
			}
		}(p.IP)
	}
	wg.Wait()
}

// PullFromBest does a snapshot pull from the best (lowest-RTT, at-or-ahead)
// peer and replays the snapshot locally.
//
// Called when local detects it's behind (heartbeat or notify-version).
// Idempotent: calling repeatedly is safe; subsequent calls are no-ops if
// already at target.
func (c *Coordinator) PullFromBest(ctx context.Context, targetVersion int64) error {
	if c.PullAndApply == nil {
		return errors.New("PullAndApply not wired")
	}
	peerIP := c.BestPeerToPull(targetVersion)
	if peerIP == "" {
		return errors.New("no eligible peer to pull from")
	}
	return c.PullAndApply(ctx, peerIP)
}
