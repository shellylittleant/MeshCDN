// Package cert manages all SSL certificates per V4-DESIGN §3.
//
// Certificates have three sources:
//   - "le":     Let's Encrypt (auto-renewable, synced cluster-wide)
//   - "upload": user-uploaded (not auto-renewable, synced cluster-wide)
//   - "self":   self-signed fallback (per-node, NOT synced, 100-year validity)
//
// All certificates live in /etc/meshcdn/persistent/certs/ named by content hash:
//
//	<sha256-prefix>.crt
//	<sha256-prefix>.key
//	manifest.json    ← metadata index
//
// manifest.json is the source of truth for cert metadata. The runtime/config.db
// `certificates` table is a query-friendly mirror, regenerated from manifest
// on boot.
//
// This package handles file storage and metadata. Selection (which cert to
// serve which endpoint) is in selector.go. Self-signed generation is in
// selfsign.go. ACME issuance lives in subpackage cert/acme/ (step 3).
package cert

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Source enumerates where a certificate came from.
type Source string

const (
	SourceLE     Source = "le"
	SourceUpload Source = "upload"
	SourceSelf   Source = "self"
)

// SourcePriority returns the precedence weight (higher = preferred).
// Per V4-DESIGN §3.6: LE > upload > self.
func SourcePriority(s Source) int {
	switch s {
	case SourceLE:
		return 3
	case SourceUpload:
		return 2
	case SourceSelf:
		return 1
	default:
		return 0
	}
}

// CertMeta is the metadata for one certificate, stored in manifest.json.
type CertMeta struct {
	FingerprintPrefix string    `json:"fingerprint_prefix"` // first 16 hex chars of SHA-256(PEM)
	FingerprintFull   string    `json:"fingerprint_sha256"` // full SHA-256 hex
	Subject           string    `json:"subject"`            // CN
	SAN               []string  `json:"san"`                // SubjectAltName entries
	Source            Source    `json:"source"`
	Issuer            string    `json:"issuer"`
	NotBefore         time.Time `json:"not_before"`
	NotAfter          time.Time `json:"not_after"`
	SelectedFor       []string  `json:"selected_for,omitempty"` // endpoints currently using this cert
}

// Manifest is the on-disk JSON structure.
type Manifest struct {
	Certificates map[string]CertMeta `json:"certificates"`
}

// Store wraps a directory of certs + their manifest.
//
// Concurrency: a single Store instance is safe to use from multiple goroutines
// thanks to the embedded mutex; metadata operations serialize through it.
// File reads (CertPEM/KeyPEM) bypass the mutex because the on-disk content
// is immutable for a given fingerprint.
type Store struct {
	dir string
	mu  sync.RWMutex
}

// DefaultDir is where certs live.
const DefaultDir = "/etc/meshcdn/persistent/certs"

// NewStore opens (or creates) a cert store at dir.
// If dir doesn't exist, it's created with mode 0700.
// If manifest.json is missing, an empty manifest is created.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create cert dir: %w", err)
	}
	s := &Store{dir: dir}
	if _, err := s.loadManifest(); err != nil {
		// Create empty manifest if missing
		if errors.Is(err, os.ErrNotExist) {
			if err := s.saveManifest(&Manifest{Certificates: map[string]CertMeta{}}); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	return s, nil
}

// Dir returns the underlying directory path.
func (s *Store) Dir() string { return s.dir }

// Add stores a new certificate. The cert and key are PEM-encoded bytes.
// Returns the resulting CertMeta with computed fingerprint.
//
// If a cert with the same fingerprint already exists, returns the existing
// meta unchanged (idempotent).
func (s *Store) Add(certPEM, keyPEM []byte, source Source) (*CertMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// V4.0.20: verify cert and key are a matched pair before doing anything
	// else. tls.X509KeyPair compares the public key embedded in the cert
	// against the private key's derived public key (modulus / curve point);
	// mismatch is the most common silent-failure mode for cert uploads
	// (user picks the wrong .key file to go with a cert) and used to only
	// surface later as a TLS handshake error in nginx.
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return nil, fmt.Errorf("cert and key do not match: %w", err)
	}

	// Parse the cert to extract subject/SAN/dates
	parsed, err := ParsePEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("parse cert: %w", err)
	}

	fp := fingerprintHex(certPEM)
	prefix := fp[:16]

	manifest, err := s.loadManifest()
	if err != nil {
		return nil, err
	}

	// Already have it?
	if existing, ok := manifest.Certificates[prefix]; ok {
		return &existing, nil
	}

	// Write files
	crtPath := filepath.Join(s.dir, prefix+".crt")
	keyPath := filepath.Join(s.dir, prefix+".key")
	if err := writeFileAtomic(crtPath, certPEM, 0600); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(keyPath, keyPEM, 0600); err != nil {
		// Best-effort cleanup
		_ = os.Remove(crtPath)
		return nil, err
	}

	meta := CertMeta{
		FingerprintPrefix: prefix,
		FingerprintFull:   fp,
		Subject:           parsed.Subject.CommonName,
		SAN:               collectSANs(parsed),
		Source:            source,
		Issuer:            parsed.Issuer.CommonName,
		NotBefore:         parsed.NotBefore,
		NotAfter:          parsed.NotAfter,
	}
	manifest.Certificates[prefix] = meta
	if err := s.saveManifest(manifest); err != nil {
		return nil, err
	}
	return &meta, nil
}

// Remove deletes a certificate by fingerprint prefix.
// Removes the .crt and .key files and the manifest entry.
func (s *Store) Remove(fingerprintPrefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	manifest, err := s.loadManifest()
	if err != nil {
		return err
	}
	if _, ok := manifest.Certificates[fingerprintPrefix]; !ok {
		return fmt.Errorf("no certificate with prefix %q", fingerprintPrefix)
	}
	delete(manifest.Certificates, fingerprintPrefix)
	if err := s.saveManifest(manifest); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(s.dir, fingerprintPrefix+".crt"))
	_ = os.Remove(filepath.Join(s.dir, fingerprintPrefix+".key"))
	return nil
}

// Get returns the metadata for one cert. Returns nil if not found.
func (s *Store) Get(fingerprintPrefix string) (*CertMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.loadManifest()
	if err != nil {
		return nil, err
	}
	if cm, ok := m.Certificates[fingerprintPrefix]; ok {
		return &cm, nil
	}
	return nil, nil
}

// All returns all certificates, sorted by fingerprint prefix for stable ordering.
func (s *Store) All() ([]CertMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.loadManifest()
	if err != nil {
		return nil, err
	}
	out := make([]CertMeta, 0, len(m.Certificates))
	for _, cm := range m.Certificates {
		out = append(out, cm)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].FingerprintPrefix < out[j].FingerprintPrefix
	})
	return out, nil
}

// Paths returns the full filesystem paths for a cert's .crt and .key files.
func (s *Store) Paths(fingerprintPrefix string) (crtPath, keyPath string) {
	return filepath.Join(s.dir, fingerprintPrefix+".crt"),
		filepath.Join(s.dir, fingerprintPrefix+".key")
}

// CertPEM returns the raw .crt PEM bytes for a cert.
func (s *Store) CertPEM(fingerprintPrefix string) ([]byte, error) {
	crtPath, _ := s.Paths(fingerprintPrefix)
	return os.ReadFile(crtPath)
}

// KeyPEM returns the raw .key PEM bytes for a cert.
func (s *Store) KeyPEM(fingerprintPrefix string) ([]byte, error) {
	_, keyPath := s.Paths(fingerprintPrefix)
	return os.ReadFile(keyPath)
}

// ─────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────

func (s *Store) manifestPath() string {
	return filepath.Join(s.dir, "manifest.json")
}

func (s *Store) loadManifest() (*Manifest, error) {
	data, err := os.ReadFile(s.manifestPath())
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Certificates == nil {
		m.Certificates = map[string]CertMeta{}
	}
	return &m, nil
}

func (s *Store) saveManifest(m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	return writeFileAtomic(s.manifestPath(), data, 0600)
}

// fingerprintHex returns the SHA-256 of the PEM bytes as lowercase hex.
func fingerprintHex(pemBytes []byte) string {
	h := sha256.Sum256(pemBytes)
	return hex.EncodeToString(h[:])
}

// ParsePEM decodes the first CERTIFICATE block from PEM bytes and parses it.
// Used internally and by selfsign/acme to verify their own output.
func ParsePEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("expected CERTIFICATE block, got %q", block.Type)
	}
	return x509.ParseCertificate(block.Bytes)
}

func collectSANs(c *x509.Certificate) []string {
	var sans []string
	sans = append(sans, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		sans = append(sans, ip.String())
	}
	return sans
}

// writeFileAtomic writes data to path via temp-file + rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
