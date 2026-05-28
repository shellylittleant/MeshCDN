// Mesh event handlers for cluster-wide ACME orchestration.
//
// Step 10 added three new event types beyond "alert":
//
//	acme-challenge-prepare:
//	  Sender: bot/issuing node, just before LE validation.
//	  Receiver: writes the token into its local challenge dir so LE can
//	  fetch it via this peer's :80, regardless of which IP LE happens
//	  to pick from the DNS A-record set.
//	  Payload: {"token": "...", "key_auth": "..."}
//
//	acme-challenge-cleanup:
//	  Sender: same node, after LE validation completes.
//	  Receiver: removes the corresponding token file.
//	  Payload: {"token": "..."}
//
//	cert-install:
//	  Sender: any node that just successfully obtained a domain cert.
//	  Receiver: writes the cert to its local cert.Store.
//	  Payload: {"cert_pem": "...", "key_pem": "...", "source": "le"}
//
// These complement the existing "alert" type which is forwarded straight
// to Telegram by the bot node.
package bot

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/example/meshcdn/internal/cert"
)

// ClusterEventHandler holds the local state needed to act on cluster events.
type ClusterEventHandler struct {
	// ChallengeDir is the directory ACME tokens land in (per node).
	// Same path used by acme.Client. Step 3 default:
	// /etc/meshcdn/runtime/challenges/.well-known/acme-challenge
	ChallengeDir string

	// CertStore receives broadcast certs.
	CertStore *cert.Store

	// AlertSink (optional): bot node uses this to forward "alert" events
	// to Telegram. Worker nodes leave this nil.
	AlertSink *TelegramAlertSink
}

// Handle is the OnEvent callback wired into mesh.Server.
func (h *ClusterEventHandler) Handle(eventType, fromIP string, payload map[string]interface{}) {
	switch eventType {
	case "alert":
		// Bot node only — forward to Telegram.
		if h.AlertSink != nil {
			h.AlertSink.HandleMeshEvent(eventType, fromIP, payload)
		}

	case "acme-challenge-prepare":
		token, _ := payload["token"].(string)
		keyAuth, _ := payload["key_auth"].(string)
		if token == "" || keyAuth == "" {
			log.Printf("[mesh-event] challenge-prepare from %s: missing token/key_auth", fromIP)
			return
		}
		if !isValidChallengeToken(token) {
			log.Printf("[mesh-event] challenge-prepare from %s: rejected suspicious token", fromIP)
			return
		}
		if h.ChallengeDir == "" {
			log.Printf("[mesh-event] challenge-prepare: ChallengeDir not configured")
			return
		}
		if err := os.MkdirAll(h.ChallengeDir, 0755); err != nil {
			log.Printf("[mesh-event] mkdir challenge: %v", err)
			return
		}
		path := filepath.Join(h.ChallengeDir, token)
		if err := os.WriteFile(path, []byte(keyAuth), 0644); err != nil {
			log.Printf("[mesh-event] write challenge: %v", err)
			return
		}
		log.Printf("[mesh-event] challenge token from %s prepared (%s)", fromIP, token[:8])

	case "acme-challenge-cleanup":
		token, _ := payload["token"].(string)
		if token == "" || !isValidChallengeToken(token) {
			return
		}
		path := filepath.Join(h.ChallengeDir, token)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[mesh-event] cleanup challenge %s: %v", token[:8], err)
		}

	case "cert-install":
		certB64, _ := payload["cert_pem"].(string)
		keyB64, _ := payload["key_pem"].(string)
		source, _ := payload["source"].(string)
		if certB64 == "" || keyB64 == "" {
			log.Printf("[mesh-event] cert-install from %s: empty cert or key", fromIP)
			return
		}
		certPEM, err := base64.StdEncoding.DecodeString(certB64)
		if err != nil {
			log.Printf("[mesh-event] cert-install decode cert: %v", err)
			return
		}
		keyPEM, err := base64.StdEncoding.DecodeString(keyB64)
		if err != nil {
			log.Printf("[mesh-event] cert-install decode key: %v", err)
			return
		}
		if h.CertStore == nil {
			log.Printf("[mesh-event] cert-install: CertStore not configured")
			return
		}
		src := cert.SourceLE
		if source == "upload" {
			src = cert.SourceUpload
		}
		meta, err := h.CertStore.Add(certPEM, keyPEM, src)
		if err != nil {
			log.Printf("[mesh-event] cert-install add: %v", err)
			return
		}
		log.Printf("[mesh-event] cert installed from %s: subject=%s fp=%s",
			fromIP, meta.Subject, meta.FingerprintPrefix)
		// Note: we don't trigger nginx reload here — the local node's renew
		// scanner / next config write will pick the new cert up via the
		// selector (V4-DESIGN §3.6). For immediate effect, operator can
		// run a /v sync.

	case "peer-joined", "peer-lost", "bot-transferred":
		// These are bot-node-only events shown in Telegram.
		if h.AlertSink != nil {
			h.AlertSink.HandleMeshEvent(eventType, fromIP, payload)
		}

	default:
		log.Printf("[mesh-event] unknown type %q from %s", eventType, fromIP)
	}
}

// isValidChallengeToken: tokens are LE-issued URL-safe base64; refuse
// anything with separators that could escape the challenge dir.
func isValidChallengeToken(t string) bool {
	if t == "" || len(t) > 256 {
		return false
	}
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// ─────────────────────────────────────────────────────────────────────
// Outbound senders — used by the issuing node
// ─────────────────────────────────────────────────────────────────────

// PostEventFunc is the contract for sending mesh events. Wired to
// mesh.Client.MakePostEvent(localIP, port) at startup time.
type PostEventFunc = func(ctx context.Context, peerIP, eventType string, payload map[string]interface{}) error

// BroadcastChallenge implements the ChallengeBroadcast hook expected by
// ssl handler. It posts an "acme-challenge-prepare" event to each peer.
//
// Returns first error if any peer rejects (best-effort otherwise — we don't
// fail the whole issuance if one peer is offline; LE will still try the
// reachable peers).
func BroadcastChallenge(post PostEventFunc) func(ctx context.Context, peerIPs []string, token, keyAuth string) error {
	return func(ctx context.Context, peerIPs []string, token, keyAuth string) error {
		for _, ip := range peerIPs {
			payload := map[string]interface{}{
				"token":    token,
				"key_auth": keyAuth,
			}
			if err := post(ctx, ip, "acme-challenge-prepare", payload); err != nil {
				return fmt.Errorf("broadcast to %s: %w", ip, err)
			}
		}
		return nil
	}
}

// CleanupChallenge issues acme-challenge-cleanup events to each peer.
func CleanupChallenge(post PostEventFunc) func(ctx context.Context, peerIPs []string, token string) {
	return func(ctx context.Context, peerIPs []string, token string) {
		for _, ip := range peerIPs {
			payload := map[string]interface{}{"token": token}
			if err := post(ctx, ip, "acme-challenge-cleanup", payload); err != nil {
				log.Printf("[mesh-event] cleanup to %s: %v", ip, err)
			}
		}
	}
}

// BroadcastCert returns a CertBroadcast hook that ships a freshly-issued
// cert to ALL cluster peers via "cert-install" events.
func BroadcastCert(post PostEventFunc, allPeers func() []string) func(ctx context.Context, certPEM, keyPEM []byte, source string) {
	return func(ctx context.Context, certPEM, keyPEM []byte, source string) {
		payload := map[string]interface{}{
			"cert_pem": base64.StdEncoding.EncodeToString(certPEM),
			"key_pem":  base64.StdEncoding.EncodeToString(keyPEM),
			"source":   source,
		}
		for _, ip := range allPeers() {
			if err := post(ctx, ip, "cert-install", payload); err != nil {
				log.Printf("[mesh-event] cert broadcast to %s: %v", ip, err)
			}
		}
	}
}
