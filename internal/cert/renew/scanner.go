// Package renew implements V4-DESIGN §3.7 — periodic certificate renewal.
//
// Two cooperating components:
//
//	Scanner: every 6 hours, walks all certs in the store. For any cert that's
//	         currently selected for some endpoint AND has < 3 days remaining,
//	         enqueues a renewal task into the worker.
//
//	Worker:  pulls tasks from the queue, runs the §3.7 four-step flow:
//	         renew → find replacement → alert admin → fallback (implicit
//	         via §3.6 selection algorithm).
//
// Both run as goroutines started from the main agent process. Single-shot
// "run once and exit" is also supported for unit tests.
//
// Per V4-DESIGN philosophy: this package surfaces problems via alerts;
// it does not silently work around failures. Expired certs that nobody
// renews → the endpoint serves whatever §3.6 picks, possibly a self-signed.
// That's correct behavior, not a bug to fix.
package renew

import (
	"context"
	"log"
	"time"

	"github.com/example/meshcdn/internal/cert"
)

// ScanInterval is how often the scanner runs. Per V4-DESIGN §3.7 = 6 hours.
const ScanInterval = 6 * time.Hour

// ExpiryThreshold is "begin trying to renew if remaining < this" = 3 days.
const ExpiryThreshold = 3 * 24 * time.Hour

// Scanner periodically inspects the cert store and enqueues renewal tasks.
type Scanner struct {
	Store  *cert.Store
	Worker *Worker

	// SelectedEndpoints reports which endpoints currently use which cert.
	// Source: typically a callback that queries the running config (db
	// `domains` + `bindings` + selector results). For step 3 single-node
	// we use a simple shim.
	//
	// Returns map: endpoint identifier → fingerprint prefix of selected cert.
	SelectedEndpoints func() (map[string]string, error)
}

// Run starts the scanner loop. Blocks until ctx is canceled.
//
// First scan happens immediately (so a freshly-started agent picks up
// any pending renewals from before it started).
func (s *Scanner) Run(ctx context.Context) {
	// Run once immediately
	if err := s.RunOnce(); err != nil {
		log.Printf("[renew/scanner] initial scan: %v", err)
	}

	ticker := time.NewTicker(ScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RunOnce(); err != nil {
				log.Printf("[renew/scanner] scan: %v", err)
			}
		}
	}
}

// RunOnce executes a single scan pass.
// Used by Run() and by tests.
func (s *Scanner) RunOnce() error {
	now := time.Now().UTC()

	all, err := s.Store.All()
	if err != nil {
		return err
	}

	// Build lookup of which fingerprints are currently selected
	selected, err := s.selectedFingerprints()
	if err != nil {
		return err
	}

	enqueued := 0
	for _, cm := range all {
		// Only act on certs that are currently selected — a cert sitting
		// in the store but not in use isn't worth renewing.
		if !selected[cm.FingerprintPrefix] {
			continue
		}
		if !cert.IsExpiring(&cm, now, ExpiryThreshold) {
			continue
		}
		// Safety: if a cert has been expired for more than 30 days, skip it.
		// It's almost certainly a stale store entry not in actual use; trying
		// to renew it would waste ACME quota. Either operator removes it
		// manually with /d ssl, or it's safe to leave.
		if !cm.NotAfter.IsZero() && now.Sub(cm.NotAfter) > 30*24*time.Hour {
			continue
		}

		// Enqueue (worker handles dedup by fingerprint)
		s.Worker.Enqueue(Task{
			Type:        TaskRenew,
			Fingerprint: cm.FingerprintPrefix,
			Identifier:  cm.Subject, // primary identifier is the subject
			Source:      cm.Source,
		})
		enqueued++
	}

	if enqueued > 0 {
		log.Printf("[renew/scanner] enqueued %d renewal tasks", enqueued)
	}
	return nil
}

// selectedFingerprints returns the set of fingerprints currently in use.
// In step 3 single-node, we approximate "selected" as "any non-self-signed
// cert plus self-signed certs whose subject matches a domain row's host".
// Step 6 will replace this with a precise query against domains+bindings.
func (s *Scanner) selectedFingerprints() (map[string]bool, error) {
	if s.SelectedEndpoints != nil {
		mapping, err := s.SelectedEndpoints()
		if err != nil {
			return nil, err
		}
		out := make(map[string]bool, len(mapping))
		for _, fp := range mapping {
			out[fp] = true
		}
		return out, nil
	}

	// Default: treat all certs as selected (overscan; conservative but safe)
	all, err := s.Store.All()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(all))
	for _, cm := range all {
		out[cm.FingerprintPrefix] = true
	}
	return out, nil
}
