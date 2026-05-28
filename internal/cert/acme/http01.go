// HTTP-01 challenge provider for ACME.
//
// Per V4-DESIGN §3.3:
//   - Negotiating node X drops the challenge token into a directory
//   - nginx (on this node and, in cluster mode, on all candidate nodes)
//     serves that directory at /.well-known/acme-challenge/<token>
//   - LE fetches the token from any IP in the DNS A record set
//
// In V4 single-node (steps 1-3): the challenge dir is local; only this
// node's nginx serves it. Works fine for domains with single A record.
//
// In V4 multi-node (step 4+): the negotiating node broadcasts the token
// to all peers in candidates(domain), and they all write it locally.
// Token cleanup happens via the Cleanup() callback after issuance completes.
package acme

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// http01Provider implements lego's challenge.Provider interface.
// It writes the challenge token to a file at <challengeDir>/<token>.
type http01Provider struct {
	challengeDir string

	// onPresent is called when a challenge token is received but before it's
	// served. In multi-node mode, this hooks into the mesh broadcast (step 4).
	// For now (single-node), it's nil.
	onPresent func(domain, token, keyAuth string) error

	// onCleanup is called after issuance to remove the challenge file
	// (and notify peers in multi-node mode).
	onCleanup func(domain, token, keyAuth string) error
}

// newHTTP01Provider returns a provider that writes challenges to challengeDir.
func newHTTP01Provider(challengeDir string) *http01Provider {
	return &http01Provider{challengeDir: challengeDir}
}

// Present is called by lego when LE tells us what challenge token to serve.
//
// We write the keyAuth string into <challengeDir>/<token>. nginx then serves
// http://<domain>/.well-known/acme-challenge/<token> by reading this file.
//
// keyAuth is what LE expects to find when it fetches the URL — it's the
// proof that we control the domain.
func (p *http01Provider) Present(domain, token, keyAuth string) error {
	if err := os.MkdirAll(p.challengeDir, 0755); err != nil {
		return fmt.Errorf("create challenge dir: %w", err)
	}
	if !isValidToken(token) {
		return errors.New("invalid challenge token (would escape challenge dir)")
	}
	tokenPath := filepath.Join(p.challengeDir, token)
	if err := os.WriteFile(tokenPath, []byte(keyAuth), 0644); err != nil {
		return fmt.Errorf("write challenge: %w", err)
	}

	if p.onPresent != nil {
		if err := p.onPresent(domain, token, keyAuth); err != nil {
			// Best-effort: clean up local file before propagating error
			_ = os.Remove(tokenPath)
			return fmt.Errorf("onPresent hook: %w", err)
		}
	}
	return nil
}

// CleanUp is called by lego after issuance completes (success or failure).
// We remove the challenge file from disk.
func (p *http01Provider) CleanUp(domain, token, keyAuth string) error {
	if !isValidToken(token) {
		return nil
	}
	tokenPath := filepath.Join(p.challengeDir, token)
	if err := os.Remove(tokenPath); err != nil && !os.IsNotExist(err) {
		// Don't fail the whole flow if cleanup hiccups; just log
		// (logging not yet wired in step 3; will be in step 4)
	}

	if p.onCleanup != nil {
		_ = p.onCleanup(domain, token, keyAuth)
	}
	return nil
}

// isValidToken sanity-checks the token to prevent path traversal.
// LE tokens are URL-safe base64 chars only.
func isValidToken(t string) bool {
	if t == "" || len(t) > 256 {
		return false
	}
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			// ok
		default:
			return false
		}
	}
	return true
}
