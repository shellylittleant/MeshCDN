// Self-signed certificate generation per V4-DESIGN §3.8.
//
// Generated at install.sh time, 100-year validity, never synced.
// Acts as the floor of the cert-source priority hierarchy
// (LE > upload > self).
//
// Used for:
//   - default_server SSL when no business cert covers an endpoint
//   - mesh communication on port 9443 between nodes (each node uses its own
//     self-signed; bearer-token auth carries identity, TLS just provides encryption)
//   - the IP endpoint when /w domain has been called for the IP but no LE/upload
//     cert is yet available
package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// GenerateSelfSigned creates a fresh self-signed cert + key pair for the
// given identity (a hostname or IP).
//
// Validity: 100 years (V4-DESIGN §3.8 — "one-time, not renewed").
// Algorithm: ECDSA P-256 (small key, fast handshake, modern).
//
// Returns PEM-encoded certificate and private key bytes, ready to feed
// into Store.Add(certPEM, keyPEM, SourceSelf).
func GenerateSelfSigned(identifier string) (certPEM, keyPEM []byte, err error) {
	return GenerateSelfSignedMulti([]string{identifier})
}

// GenerateSelfSignedMulti is like GenerateSelfSigned but covers every identifier
// (any mix of DNS names and IPs) in the SAN. Renewal uses this so re-generating
// a self-signed cert preserves its full SAN set instead of shrinking to the CN
// (V4-DESIGN §3.7). The CommonName is the first identifier.
func GenerateSelfSignedMulti(identifiers []string) (certPEM, keyPEM []byte, err error) {
	if len(identifiers) == 0 {
		return nil, nil, fmt.Errorf("no identifiers for self-signed cert")
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	// Split identifiers into IPs and hostnames for the SAN.
	var ipSAN []net.IP
	var dnsSAN []string
	for _, id := range identifiers {
		if ip := net.ParseIP(id); ip != nil {
			ipSAN = append(ipSAN, ip)
		} else {
			dnsSAN = append(dnsSAN, id)
		}
	}

	now := time.Now().UTC()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   identifiers[0],
			Organization: []string{"MeshCDN Self-Signed"},
		},
		NotBefore:             now.Add(-1 * time.Hour), // small leeway for clock skew
		NotAfter:              now.AddDate(100, 0, 0),  // 100 years
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		IPAddresses:           ipSAN,
		DNSNames:              dnsSAN,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}

	// Encode cert
	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: derBytes,
	})

	// Encode key
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})

	return certPEM, keyPEM, nil
}

// EnsureSelfSigned makes sure the given Store has a self-signed cert
// covering the given identifier. If one exists, returns the existing
// CertMeta. Otherwise generates a new one and adds it.
//
// This is called by install.sh's setup phase and at agent startup as a
// belt-and-suspenders check (per V4-DESIGN §3.8, install.sh is the
// "唯一责任主体" — but a fresh-start agent may have lost its self-signed
// due to operator error, so we tolerate that case here).
//
// Note: V4-DESIGN §4.3 says agent boot does NOT do "自签兜底检查". This
// function is for install.sh's use, not boot. Agent boot should fail
// loudly if no self-signed exists, per the design philosophy
// "程序的边界 = 信息边界" — if install.sh didn't generate one, that's a
// sign something is wrong; don't silently fix it.
func EnsureSelfSigned(s *Store, identifier string) (*CertMeta, error) {
	// Check if any existing cert with source=self covers this identifier
	existing, err := s.All()
	if err != nil {
		return nil, err
	}
	for _, cm := range existing {
		if cm.Source != SourceSelf {
			continue
		}
		// Match by subject or SAN
		if cm.Subject == identifier {
			return &cm, nil
		}
		for _, san := range cm.SAN {
			if san == identifier {
				return &cm, nil
			}
		}
	}

	// Generate new
	certPEM, keyPEM, err := GenerateSelfSigned(identifier)
	if err != nil {
		return nil, err
	}
	return s.Add(certPEM, keyPEM, SourceSelf)
}
