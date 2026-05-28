// PEM content sniffing — detect whether an uploaded file is a certificate
// or a private key based on its PEM header, not its filename.
//
// V4.0.20 addition. Replaces the v4.0.19 suffix-based dispatch which
// incorrectly classified key.pem (suffix .pem) as a certificate.
//
// The sniff is deliberately tolerant:
//   - Leading whitespace / BOM / noise lines are skipped (pem.Decode does this)
//   - Filename and extension are completely ignored
//   - Only the first PEM block is consulted (a v4.0.21 enhancement could
//     fan out multi-block "bundle" files into cert+key in one shot)
package bot

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// pemSniffResult describes what a sniffed file looks like.
type pemSniffResult struct {
	Slot string // "cert" or "key"
	Type string // raw PEM block type, for diagnostic messages
}

// sniffPEMSlot decodes the first PEM block in data and classifies it.
//
// Returns slot="cert" for any CERTIFICATE-family block, slot="key" for any
// PRIVATE KEY-family block, or an error if the file isn't PEM or the type
// is unrecognized / unsupported.
//
// Encrypted private keys are rejected with a specific error so the upstream
// caller can tell the user how to decrypt them — we don't want to ask for
// the passphrase over a chat protocol.
func sniffPEMSlot(data []byte) (*pemSniffResult, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("file does not contain a recognizable PEM block")
	}
	typ := strings.ToUpper(strings.TrimSpace(block.Type))

	switch {
	case typ == "CERTIFICATE", typ == "TRUSTED CERTIFICATE", typ == "X509 CERTIFICATE":
		return &pemSniffResult{Slot: "cert", Type: block.Type}, nil

	case typ == "ENCRYPTED PRIVATE KEY":
		// PKCS#8 encrypted — common when openssl is used with a passphrase.
		// Refuse explicitly; chat upload of plaintext passphrases is not
		// something we want to design for.
		return nil, errors.New("encrypted private key not supported; " +
			"decrypt first locally: openssl pkcs8 -topk8 -nocrypt -in encrypted.key -out plain.key")

	case typ == "PRIVATE KEY",
		typ == "RSA PRIVATE KEY",
		typ == "EC PRIVATE KEY",
		typ == "DSA PRIVATE KEY",
		typ == "ED25519 PRIVATE KEY":
		return &pemSniffResult{Slot: "key", Type: block.Type}, nil
	}

	return nil, fmt.Errorf("PEM block type %q is not a certificate or private key", block.Type)
}

// looksLikePEM is a cheap pre-check: does the file contain any "-----BEGIN "
// marker at all? Used to decide whether an uncaptioned upload should be
// routed to the SSL-upload path (PEM-shaped) vs the config-import path
// (line-based commands).
func looksLikePEM(data []byte) bool {
	// Search-by-substring is fine; PEM markers are deterministic.
	return strings.Contains(string(data), "-----BEGIN ")
}

// extractCertCN returns the Common Name from the first CERTIFICATE block
// found in data, or "" if none. Used to auto-synthesize a scope for
// uncaptioned cert uploads. If the cert has no CN, the caller should fall
// back to asking the user to provide one via caption.
//
// We pull this out as a separate helper rather than reusing cert.ParsePEM
// because importing internal/cert from internal/bot is fine (no cycle) but
// keeping the dependency surface small here makes the bot package easier
// to reason about in isolation.
func extractCertCN(data []byte) string {
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return ""
		}
		typ := strings.ToUpper(block.Type)
		if !strings.Contains(typ, "CERTIFICATE") {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if c.Subject.CommonName != "" {
			return c.Subject.CommonName
		}
		// No CN — fall back to first DNS SAN if present.
		if len(c.DNSNames) > 0 {
			return c.DNSNames[0]
		}
		return ""
	}
}
