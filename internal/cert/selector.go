// Certificate selection algorithm per V4-DESIGN §3.6.
//
// Given an endpoint (a hostname or IP), pick the best cert from candidates:
//
//	candidates = all certs covering the endpoint that aren't expired
//	rank by:
//	  1. Source priority: LE > upload > self
//	  2. Within same source: latest expiry wins
//
// Selection stickiness:
//   - Once a cert is selected for an endpoint, do not switch as long as
//     it's still valid (avoid handshake churn).
//   - When the selected cert expires, immediately re-run the algorithm.
//
// This selector is stateless — it computes the best cert from current candidates.
// "Stickiness" is enforced by callers comparing previous selection to new one
// and only switching when the previous is invalid.
package cert

import (
	"sort"
	"strings"
	"time"
)

// Endpoint is the thing we're selecting a cert for. Currently a single
// hostname or IP literal. In the future could carry port info if needed.
type Endpoint string

// SelectFor returns the best certificate for the given endpoint, or nil
// if no candidate covers it.
//
// The endpoint matches a cert when:
//   - cert.Subject equals endpoint, OR
//   - any cert.SAN entry equals endpoint, OR
//   - cert.SAN contains a wildcard "*.X" and endpoint = "Y.X"
func SelectFor(endpoint Endpoint, candidates []CertMeta, now time.Time) *CertMeta {
	matches := FilterCovering(endpoint, candidates, now)
	if len(matches) == 0 {
		return nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return betterCert(matches[i], matches[j])
	})
	return &matches[0]
}

// FilterCovering returns the subset of candidates that:
//  1. cover endpoint (subject/SAN match including wildcards), AND
//  2. are not expired at `now`.
func FilterCovering(endpoint Endpoint, candidates []CertMeta, now time.Time) []CertMeta {
	out := make([]CertMeta, 0, len(candidates))
	for _, c := range candidates {
		if !c.NotAfter.IsZero() && now.After(c.NotAfter) {
			continue // expired
		}
		if Covers(&c, endpoint) {
			out = append(out, c)
		}
	}
	return out
}

// Covers reports whether c is valid for the given endpoint.
// Wildcard rule: a SAN of "*.example.com" covers "foo.example.com" but NOT
// "example.com" itself nor "a.b.example.com".
func Covers(c *CertMeta, endpoint Endpoint) bool {
	target := string(endpoint)
	if c.Subject == target {
		return true
	}
	for _, san := range c.SAN {
		if san == target {
			return true
		}
		if strings.HasPrefix(san, "*.") {
			suffix := san[1:] // ".example.com"
			// foo.example.com → must end with .example.com AND have exactly one
			// label before that suffix.
			if strings.HasSuffix(target, suffix) {
				prefix := target[:len(target)-len(suffix)]
				if prefix != "" && !strings.Contains(prefix, ".") {
					return true
				}
			}
		}
	}
	return false
}

// betterCert is the comparator: returns true if a is preferred over b.
func betterCert(a, b CertMeta) bool {
	pa, pb := SourcePriority(a.Source), SourcePriority(b.Source)
	if pa != pb {
		return pa > pb
	}
	// Same source: later expiry preferred
	return a.NotAfter.After(b.NotAfter)
}

// IsExpiring reports whether the cert will expire within the threshold.
// Used by the renewal scanner per V4-DESIGN §3.7 (3-day threshold).
func IsExpiring(c *CertMeta, now time.Time, threshold time.Duration) bool {
	if c.NotAfter.IsZero() {
		return false
	}
	return c.NotAfter.Sub(now) < threshold
}
