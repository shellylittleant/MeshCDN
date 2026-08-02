// Package acme wraps an ACME client (go-acme/lego) for Let's Encrypt issuance.
//
// Per V4-DESIGN §3:
//   - HTTP-01 validation (default for V4)
//   - Both domain names and IP addresses can be issued (LE supports IP since 2025-07)
//   - Issued certs are stored via cert.Store with source=le
//
// This package does NOT decide WHEN to issue (that's the SSL handler in
// commands) or HOW to validate (that's wired by http01.go which writes
// challenge files to a path served by nginx).
//
// Concurrency: a single Client can be used from multiple goroutines for
// issuance. Each issuance is independent.
package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"
)

// CADirectory is the LE directory URL. Override for staging during testing.
//
//	Production:  https://acme-v02.api.letsencrypt.org/directory
//	Staging:     https://acme-staging-v02.api.letsencrypt.org/directory
//
// V4 default is production; tests can switch to staging via env var
// MESHCDN_ACME_DIRECTORY.
var (
	CADirectoryProduction = "https://acme-v02.api.letsencrypt.org/directory"
	CADirectoryStaging    = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// DefaultDirectory returns the directory URL — production unless the
// MESHCDN_ACME_DIRECTORY env var overrides it.
func DefaultDirectory() string {
	if v := os.Getenv("MESHCDN_ACME_DIRECTORY"); v != "" {
		return v
	}
	return CADirectoryProduction
}

// Client is the high-level ACME interface used by handlers and the renewal worker.
type Client struct {
	// AccountKey is the account-level private key (one per node, persisted
	// alongside other config). Used by lego for ACME registration + renewals.
	accountKey crypto.PrivateKey

	// Email used during ACME registration. Optional; LE accepts blank email
	// but recommends one for renewal notices. We populate from identity.json
	// or leave empty.
	email string

	// challengeDir is the FILESYSTEM directory where this package writes
	// challenge tokens. nginx must be configured to serve URLs like
	// /.well-known/acme-challenge/<token> by reading <challengeDir>/<token>.
	//
	// The recommended setup (per generator templates) is:
	//   challengeDir          = /etc/meshcdn/runtime/challenges/.well-known/acme-challenge
	//   nginx root            = /etc/meshcdn/runtime/challenges
	//   nginx location ^~     = /.well-known/acme-challenge/  (with try_files $uri =404)
	// so a request for /.well-known/acme-challenge/X is served from
	// /etc/meshcdn/runtime/challenges/.well-known/acme-challenge/X — which
	// is where this package writes it.
	challengeDir string

	// Cached lego client (lazy init on first use)
	mu        sync.Mutex
	legoCache *lego.Client
}

// NewClient returns a new ACME client backed by the given account key file.
//
// If the account key file does not exist, a new ECDSA P-256 key is generated
// and saved. This key represents the node's ACME account; losing it means
// the next issuance will create a fresh ACME account (not a problem in practice).
func NewClient(accountKeyPath, email, challengeDir string) (*Client, error) {
	key, err := loadOrGenerateAccountKey(accountKeyPath)
	if err != nil {
		return nil, err
	}
	return &Client{
		accountKey:   key,
		email:        email,
		challengeDir: challengeDir,
	}, nil
}

// Issue requests a new certificate from LE for the given identifier.
// identifier may be a domain name OR an IP address (LE supports both since 2025-07).
//
// Returns the issued certificate and private key as PEM bytes, ready to feed
// into cert.Store.Add.
//
// HTTP-01 validation requires:
//   - The challenge directory writable by this process
//   - nginx serving that directory at /.well-known/acme-challenge/* on port 80
//   - DNS for the identifier pointing at this node (or, in cluster mode,
//     at any peer in the candidate set with shared challenge directory)
func (c *Client) Issue(ctx context.Context, identifier string) (certPEM, keyPEM []byte, err error) {
	return c.IssueWithHooks(ctx, identifier, nil, nil)
}

// IssueMulti issues a single certificate whose SAN covers EVERY identifier
// (any mix of DNS names and IPs). This is what renewal uses so a multi-domain
// cert is not silently shrunk to its CN (V4-DESIGN §3.7). No cluster hooks.
func (c *Client) IssueMulti(ctx context.Context, identifiers []string) (certPEM, keyPEM []byte, err error) {
	return c.IssueMultiWithHooks(ctx, identifiers, nil, nil)
}

// IssueWithHooks is the routed-issue path. The hooks let callers participate
// in HTTP-01 challenge orchestration:
//
//	onPresent(domain, token, keyAuth): called BEFORE LE starts validating.
//	  Use this to push the challenge token to other cluster nodes that
//	  LE might query (DNS multi-A scenarios). Return non-nil to abort.
//
//	onCleanup(domain, token, keyAuth): called AFTER validation completes
//	  (success or failure). Use to remove pushed tokens from peers.
//
// nil hooks → behaves exactly like Issue (single-node mode).
func (c *Client) IssueWithHooks(
	ctx context.Context,
	identifier string,
	onPresent func(domain, token, keyAuth string) error,
	onCleanup func(domain, token, keyAuth string) error,
) (certPEM, keyPEM []byte, err error) {
	return c.IssueMultiWithHooks(ctx, []string{identifier}, onPresent, onCleanup)
}

// IssueMultiWithHooks is the multi-SAN core. It issues ONE certificate whose
// SAN covers every identifier (DNS names and/or IPs), with optional cluster
// HTTP-01 hooks. Single-identifier issuance (Issue/IssueWithHooks) is just the
// len==1 case; renewal passes the expiring cert's full SAN set so the cert is
// re-issued intact rather than collapsed to its CN (V4-DESIGN §3.7).
func (c *Client) IssueMultiWithHooks(
	ctx context.Context,
	identifiers []string,
	onPresent func(domain, token, keyAuth string) error,
	onCleanup func(domain, token, keyAuth string) error,
) (certPEM, keyPEM []byte, err error) {

	if len(identifiers) == 0 {
		return nil, nil, errors.New("no identifiers to issue")
	}

	// Split identifiers into DNS names and IPs.
	var dnsNames []string
	var ips []net.IP
	for _, id := range identifiers {
		if ip := net.ParseIP(id); ip != nil {
			ips = append(ips, ip)
		} else {
			dnsNames = append(dnsNames, id)
		}
	}

	leClient, err := c.getLegoClient()
	if err != nil {
		return nil, nil, fmt.Errorf("init lego client: %w", err)
	}

	provider := newHTTP01Provider(c.challengeDir)
	provider.onPresent = onPresent
	provider.onCleanup = onCleanup
	if err := leClient.Challenge.SetHTTP01Provider(provider); err != nil {
		return nil, nil, fmt.Errorf("set http01 provider: %w", err)
	}

	// Any IP in the SAN set forces the CSR path + shortlived profile: LE
	// requires the shortlived profile for IP certs and rejects a CN holding
	// an IP literal (badCSR). Pure-DNS certs take lego's standard Obtain path.
	if len(ips) > 0 {
		return c.issueForCSR(leClient, dnsNames, ips)
	}

	req := certificate.ObtainRequest{
		Domains: dnsNames,
		Bundle:  true,
	}
	resource, err := leClient.Certificate.Obtain(req)
	if err != nil {
		return nil, nil, fmt.Errorf("obtain cert for %v: %w", dnsNames, err)
	}
	if len(resource.Certificate) == 0 || len(resource.PrivateKey) == 0 {
		return nil, nil, errors.New("ACME returned empty cert or key")
	}
	return resource.Certificate, resource.PrivateKey, nil
}

// issueForCSR builds a CSR with empty CommonName carrying all DNS names and IPs
// in SAN, then calls ObtainForCSR with profile=shortlived. Empty CN bypasses
// lego's default of putting the first identifier into the CN — LE rejects a CN
// containing an IP literal (badCSR). Used whenever the SAN set includes ≥1 IP.
func (c *Client) issueForCSR(leClient *lego.Client, dnsNames []string, ips []net.IP) (certPEM, keyPEM []byte, err error) {
	if len(ips) == 0 && len(dnsNames) == 0 {
		return nil, nil, errors.New("issueForCSR: no identifiers")
	}

	// Generate a fresh ECDSA P-256 private key for this cert
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate cert key: %w", err)
	}

	// Build a CSR: empty Subject, all identifiers in SAN
	template := x509.CertificateRequest{
		Subject:     pkix.Name{}, // intentionally empty — no CommonName
		DNSNames:    dnsNames,
		IPAddresses: ips,
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &template, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create CSR: %w", err)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse our own CSR: %w", err)
	}

	req := certificate.ObtainForCSRRequest{
		CSR:     csr,
		Bundle:  true,
		Profile: "shortlived", // LE policy: IP certs require shortlived profile
	}
	resource, err := leClient.Certificate.ObtainForCSR(req)
	if err != nil {
		return nil, nil, fmt.Errorf("obtain cert for dns=%v ip=%v: %w", dnsNames, ips, err)
	}
	if len(resource.Certificate) == 0 {
		return nil, nil, errors.New("ACME returned empty cert")
	}

	// ObtainForCSR doesn't return the private key (we provided the CSR,
	// keeping the key ourselves). Encode our private key as PEM.
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})

	return resource.Certificate, keyPEM, nil
}

// getLegoClient lazy-initializes the underlying lego.Client.
// Account registration happens here on first call; subsequent calls reuse.
func (c *Client) getLegoClient() (*lego.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.legoCache != nil {
		return c.legoCache, nil
	}

	user := &acmeUser{Email: c.email, Key: c.accountKey}
	cfg := lego.NewConfig(user)
	cfg.CADirURL = DefaultDirectory()
	cfg.Certificate.KeyType = certcrypto.EC256

	cl, err := lego.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("new lego client: %w", err)
	}

	reg, err := cl.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, fmt.Errorf("acme register: %w", err)
	}
	user.Registration = reg

	c.legoCache = cl
	return cl, nil
}

// ─────────────────────────────────────────────────────────────────────
// Account key persistence
// ─────────────────────────────────────────────────────────────────────

func loadOrGenerateAccountKey(path string) (crypto.PrivateKey, error) {
	// Try load
	if data, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("account key file %s is not PEM", path)
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse account key: %w", err)
		}
		return key, nil
	}

	// Generate new
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate account key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal account key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: der,
	})
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("mkdir for account key: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pemBytes, 0600); err != nil {
		return nil, fmt.Errorf("write account key: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("rename account key: %w", err)
	}
	return key, nil
}

// acmeUser implements lego's User interface.
type acmeUser struct {
	Email        string
	Registration *registration.Resource
	Key          crypto.PrivateKey
}

func (u *acmeUser) GetEmail() string                        { return u.Email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.Registration }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.Key }
