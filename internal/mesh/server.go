// Package mesh implements the cluster's peer-to-peer protocol.
//
// V4-DESIGN §1 (sync mechanism), §1.6 (dual-stream protocol), §5.1 (auth).
//
// UPDATED for step 5: Mux is exposed so callers can register additional
// routes (upgrade endpoints) before Start.
package mesh

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const DefaultPort = 9443
const AuthHeader = "Authorization"

type HandshakeRequest struct {
	NodeIP string `json:"node_ip"`
}
type HandshakeResponse struct {
	OK        bool       `json:"ok"`
	JoinOrder int        `json:"join_order"`
	Peers     []PeerInfo `json:"peers"`
	Snapshot  string     `json:"snapshot"`
	BotNodeIP string     `json:"bot_node"`
	Reason    string     `json:"reason,omitempty"`
}
type PeerInfo struct {
	IP        string `json:"ip"`
	JoinOrder int    `json:"join_order"`
}
type PingRequest struct {
	NodeIP         string `json:"node_ip"`
	ConfigVersion  int64  `json:"config_version"`
	ProgramVersion string `json:"program_version"`
	PeerKnownCount int    `json:"peer_known_count"`
}
type PingResponse struct {
	NodeIP         string `json:"node_ip"`
	ConfigVersion  int64  `json:"config_version"`
	ProgramVersion string `json:"program_version"`
	PeerKnownCount int    `json:"peer_known_count"`
}
type NotifyVersionRequest struct {
	FromIP        string `json:"from_ip"`
	ConfigVersion int64  `json:"config_version"`
}
type NotifyVersionResponse struct {
	OK bool `json:"ok"`
}
type EventMessage struct {
	FromIP  string                 `json:"from_ip"`
	Type    string                 `json:"type"`
	Payload map[string]interface{} `json:"payload"`
}

// ExecRequest carries a command to be executed remotely on the receiving peer.
//
// Per V4 step 10 design: bot node uses this to delegate /w ssl <id>
// to the node that actually owns the IP/domain, so HTTP-01 validation
// happens on the right machine. Also used by /v sync, /v upgrade, and
// the renew worker (when it needs to delegate work).
type ExecRequest struct {
	FromIP  string `json:"from_ip"`
	Command string `json:"command"` // single-line command, e.g. "/w ssl 1.2.3.4 -"
}

// ExecResponse carries the result of a remote execution back to the caller.
//
// Output is the executor's formatted report (same as you'd see in CLI/Telegram).
// OK=false means the command(s) ran but at least one failed; Output explains why.
// HTTP-level failures (auth, network) surface through the regular HTTP error path.
type ExecResponse struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Reason string `json:"reason,omitempty"`
}

type Server struct {
	Token       string
	LocalNodeIP string
	Port        int
	CertFile    string
	KeyFile     string

	OnHandshake     func(r *HandshakeRequest) (*HandshakeResponse, error)
	OnPing          func(r *PingRequest) (*PingResponse, error)
	OnSnapshotPull  func() (snapshotText string, err error)
	OnNotifyVersion func(r *NotifyVersionRequest) error
	OnEvent         func(r *EventMessage) error
	OnExec          func(r *ExecRequest) (*ExecResponse, error)

	// Mux is exposed so callers can register additional routes (e.g. upgrade)
	// before calling Start. Initialized lazily by getMux() if nil.
	Mux *http.ServeMux

	server *http.Server
}

// NewServer returns a Server with an initialized mux. Recommended for
// callers that want to register upgrade routes:
//
//	srv := mesh.NewServer()
//	srv.Token = ...
//	srv.AddUpgradeRoutes("/usr/local/bin/cdn-agent", srv.Mux)
//	srv.Start()
func NewServer() *Server {
	return &Server{Mux: http.NewServeMux()}
}

func (s *Server) getMux() *http.ServeMux {
	if s.Mux == nil {
		s.Mux = http.NewServeMux()
	}
	return s.Mux
}

func (s *Server) Start() error {
	if s.Port == 0 {
		s.Port = DefaultPort
	}

	mux := s.getMux()
	mux.HandleFunc("/mesh/auth", s.handleAuth)
	mux.HandleFunc("/mesh/ping", s.handlePing)
	mux.HandleFunc("/mesh/snapshot-pull", s.handleSnapshotPull)
	mux.HandleFunc("/mesh/notify-version", s.handleNotifyVersion)
	mux.HandleFunc("/mesh/event", s.handleEvent)
	mux.HandleFunc("/mesh/exec", s.handleExec)
	// Note: /mesh/binary and /mesh/upgrade registered separately via AddUpgradeRoutes.

	s.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.Port),
		Handler:           mux,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", s.server.Addr, err)
	}
	if _, err := os.Stat(s.CertFile); err != nil {
		_ = listener.Close()
		return fmt.Errorf("cert file %s: %w", s.CertFile, err)
	}
	if _, err := os.Stat(s.KeyFile); err != nil {
		_ = listener.Close()
		return fmt.Errorf("key file %s: %w", s.KeyFile, err)
	}

	log.Printf("[mesh/server] listening on :%d (TLS)", s.Port)
	go func() {
		if err := s.server.ServeTLS(listener, s.CertFile, s.KeyFile); err != nil && err != http.ErrServerClosed {
			log.Printf("[mesh/server] serve error: %v", err)
		}
	}()
	return nil
}

func (s *Server) Stop() error {
	if s.server == nil {
		return nil
	}
	return s.server.Close()
}

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	hdr := r.Header.Get(AuthHeader)
	if hdr == "" {
		http.Error(w, "missing Authorization header", http.StatusUnauthorized)
		return false
	}
	if !strings.HasPrefix(hdr, "Bearer ") {
		http.Error(w, "invalid Authorization scheme", http.StatusUnauthorized)
		return false
	}
	tok := strings.TrimPrefix(hdr, "Bearer ")
	if tok != s.Token {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	var req HandshakeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.OnHandshake == nil {
		http.Error(w, "handshake not implemented", http.StatusNotImplemented)
		return
	}
	resp, err := s.OnHandshake(&req)
	if err != nil {
		log.Printf("[mesh/server] handshake error: %v", err)
		writeJSON(w, http.StatusInternalServerError, &HandshakeResponse{OK: false, Reason: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	var req PingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.OnPing == nil {
		http.Error(w, "ping not implemented", http.StatusNotImplemented)
		return
	}
	resp, err := s.OnPing(&req)
	if err != nil {
		log.Printf("[mesh/server] ping error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSnapshotPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	if s.OnSnapshotPull == nil {
		http.Error(w, "snapshot-pull not implemented", http.StatusNotImplemented)
		return
	}
	text, err := s.OnSnapshotPull()
	if err != nil {
		log.Printf("[mesh/server] snapshot-pull error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, text)
}

func (s *Server) handleNotifyVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	var req NotifyVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.OnNotifyVersion != nil {
		if err := s.OnNotifyVersion(&req); err != nil {
			log.Printf("[mesh/server] notify-version handler: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, &NotifyVersionResponse{OK: true})
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	var msg EventMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.OnEvent != nil {
		if err := s.OnEvent(&msg); err != nil {
			log.Printf("[mesh/server] event handler: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.OnExec == nil {
		writeJSON(w, http.StatusNotImplemented, &ExecResponse{
			OK: false, Reason: "exec not implemented on this peer",
		})
		return
	}
	resp, err := s.OnExec(&req)
	if err != nil {
		log.Printf("[mesh/server] exec error: %v", err)
		writeJSON(w, http.StatusInternalServerError, &ExecResponse{
			OK: false, Reason: err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
