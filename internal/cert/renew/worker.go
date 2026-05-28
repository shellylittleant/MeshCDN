// Renewal worker — implements V4-DESIGN §3.7 four-step renewal flow.
//
//	Step 0: classify (LE/self → renew; upload → skip to step 2)
//	Step 1: attempt renewal
//	Step 2: find non-self replacement
//	Step 3: alert admin (24h dedup)
//	Step 4: implicit (§3.6 selection algorithm handles expiry naturally)
//
// Tasks are queued in memory (unbuffered channel + dedup map). Process
// restart drops the queue; next scanner run rediscovers expiring certs
// and re-enqueues. This is the deliberate simplicity per V4-DESIGN.
package renew

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/example/meshcdn/internal/cert"
	"github.com/example/meshcdn/internal/cert/acme"
)

// TaskType enumerates the kinds of renewal tasks.
type TaskType string

const (
	TaskRenew           TaskType = "renew"
	TaskFindReplacement TaskType = "find-replacement"
	TaskAlertAdmin      TaskType = "alert-admin"
)

// Task is a unit of work in the renewal queue.
type Task struct {
	Type        TaskType
	Fingerprint string      // current cert's fingerprint prefix
	Identifier  string      // domain or IP for which cert is needed
	Source      cert.Source // current cert's source (LE/upload/self)

	// Carried forward across step transitions
	LastError error
}

// AlertSink receives admin-facing alerts (V4-DESIGN §3.7 step 3).
//
// In step 7 (Telegram bot), this is wired to send alerts to the group.
// In CLI/test mode, a logger sink prints to stderr.
type AlertSink interface {
	Alert(identifier, source string, notAfter time.Time, errMsg string)
}

// LoggerSink is the default AlertSink — writes alerts to standard log.
type LoggerSink struct{}

func (LoggerSink) Alert(identifier, source string, notAfter time.Time, errMsg string) {
	log.Printf("[ALERT] cert renewal failed: identifier=%s source=%s expires=%s error=%s",
		identifier, source, notAfter.Format(time.RFC3339), errMsg)
}

// Worker is the renewal task processor.
type Worker struct {
	Store      *cert.Store
	ACMEClient *acme.Client
	AlertSink  AlertSink

	// Concurrency: how many tasks process in parallel.
	// 2-4 is reasonable for a small cluster; ACME has rate limits anyway.
	Concurrency int

	queue   chan Task
	pending sync.Map // fingerprint → struct{} dedup set
	alerted sync.Map // fingerprint → time.Time of last alert
}

// NewWorker constructs a Worker with sensible defaults.
func NewWorker(store *cert.Store, acmeClient *acme.Client, sink AlertSink) *Worker {
	if sink == nil {
		sink = LoggerSink{}
	}
	return &Worker{
		Store:       store,
		ACMEClient:  acmeClient,
		AlertSink:   sink,
		Concurrency: 2,
		queue:       make(chan Task, 64),
	}
}

// Enqueue adds a task to the queue. Dedups by (Type, Fingerprint).
// Returns true if enqueued, false if already pending.
func (w *Worker) Enqueue(t Task) bool {
	key := string(t.Type) + ":" + t.Fingerprint
	if _, loaded := w.pending.LoadOrStore(key, struct{}{}); loaded {
		return false
	}
	select {
	case w.queue <- t:
		return true
	default:
		// Queue full; drop and clear pending so retry can re-enqueue.
		w.pending.Delete(key)
		log.Printf("[renew/worker] queue full, dropped task for %s", t.Fingerprint)
		return false
	}
}

// Run starts worker goroutines. Blocks until ctx is canceled.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < w.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runOne(ctx)
		}()
	}
	wg.Wait()
}

func (w *Worker) runOne(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-w.queue:
			w.dispatch(ctx, task)
		}
	}
}

// dispatch routes a task to the appropriate handler based on its Type.
func (w *Worker) dispatch(ctx context.Context, task Task) {
	defer func() {
		// Clear pending once task fully done (whether success or failure)
		key := string(task.Type) + ":" + task.Fingerprint
		w.pending.Delete(key)
	}()

	switch task.Type {
	case TaskRenew:
		w.stepRenew(ctx, task)
	case TaskFindReplacement:
		w.stepFindReplacement(ctx, task)
	case TaskAlertAdmin:
		w.stepAlertAdmin(ctx, task)
	}
}

// Step 1: attempt renewal.
func (w *Worker) stepRenew(ctx context.Context, task Task) {
	switch task.Source {
	case cert.SourceLE:
		if w.ACMEClient == nil {
			task.LastError = errors.New("ACME client not configured")
			w.Enqueue(Task{Type: TaskFindReplacement, Fingerprint: task.Fingerprint,
				Identifier: task.Identifier, Source: task.Source, LastError: task.LastError})
			return
		}
		certPEM, keyPEM, err := w.ACMEClient.Issue(ctx, task.Identifier)
		if err != nil {
			task.LastError = err
			log.Printf("[renew/worker] LE renewal failed for %s: %v", task.Identifier, err)
			w.Enqueue(Task{Type: TaskFindReplacement, Fingerprint: task.Fingerprint,
				Identifier: task.Identifier, Source: task.Source, LastError: err})
			return
		}
		if _, err := w.Store.Add(certPEM, keyPEM, cert.SourceLE); err != nil {
			log.Printf("[renew/worker] LE renewal stored but Add failed: %v", err)
		}
		log.Printf("[renew/worker] LE renewed for %s", task.Identifier)
		// Success — selector will pick the new cert on next selection.
		// We do NOT remove the old cert; selector picks the latest expiry naturally.
		return

	case cert.SourceSelf:
		// Regenerate self-signed (cheap)
		certPEM, keyPEM, err := cert.GenerateSelfSigned(task.Identifier)
		if err != nil {
			task.LastError = err
			log.Printf("[renew/worker] self-sign regenerate failed for %s: %v", task.Identifier, err)
			w.Enqueue(Task{Type: TaskFindReplacement, Fingerprint: task.Fingerprint,
				Identifier: task.Identifier, Source: task.Source, LastError: err})
			return
		}
		if _, err := w.Store.Add(certPEM, keyPEM, cert.SourceSelf); err != nil {
			log.Printf("[renew/worker] self-sign stored but Add failed: %v", err)
		}
		return

	case cert.SourceUpload:
		// V4-DESIGN §3.7 step 0: upload certs cannot be renewed.
		// Skip directly to find-replacement.
		task.LastError = errors.New("upload-source certs are not auto-renewable")
		w.Enqueue(Task{Type: TaskFindReplacement, Fingerprint: task.Fingerprint,
			Identifier: task.Identifier, Source: task.Source, LastError: task.LastError})
		return
	}
}

// Step 2: find a non-self replacement with later expiry.
func (w *Worker) stepFindReplacement(ctx context.Context, task Task) {
	all, err := w.Store.All()
	if err != nil {
		log.Printf("[renew/worker] load store: %v", err)
		w.Enqueue(Task{Type: TaskAlertAdmin, Fingerprint: task.Fingerprint,
			Identifier: task.Identifier, Source: task.Source, LastError: err})
		return
	}
	current, err := w.Store.Get(task.Fingerprint)
	if err != nil || current == nil {
		// Current cert vanished — nothing to do
		return
	}

	var bestReplacement *cert.CertMeta
	for i, cm := range all {
		if cm.Source == cert.SourceSelf {
			continue
		}
		if cm.FingerprintPrefix == current.FingerprintPrefix {
			continue
		}
		if !cert.Covers(&cm, cert.Endpoint(task.Identifier)) {
			continue
		}
		if !cm.NotAfter.After(current.NotAfter) {
			continue
		}
		if bestReplacement == nil || cm.NotAfter.After(bestReplacement.NotAfter) {
			bestReplacement = &all[i]
		}
	}

	if bestReplacement != nil {
		log.Printf("[renew/worker] auto-switched %s to replacement cert %s",
			task.Identifier, bestReplacement.FingerprintPrefix)
		// The selector will pick the replacement next time. We don't need
		// to do anything else — V4-DESIGN's selector is stateless and
		// re-runs on each request.
		return
	}

	// No replacement → alert admin
	w.Enqueue(Task{Type: TaskAlertAdmin, Fingerprint: task.Fingerprint,
		Identifier: task.Identifier, Source: task.Source, LastError: task.LastError})
}

// Step 3: alert admin, with 24h dedup per fingerprint.
func (w *Worker) stepAlertAdmin(_ context.Context, task Task) {
	now := time.Now().UTC()
	if last, ok := w.alerted.Load(task.Fingerprint); ok {
		if t, ok2 := last.(time.Time); ok2 && now.Sub(t) < 24*time.Hour {
			return // deduplicated
		}
	}
	w.alerted.Store(task.Fingerprint, now)

	current, err := w.Store.Get(task.Fingerprint)
	notAfter := time.Time{}
	if err == nil && current != nil {
		notAfter = current.NotAfter
	}
	errMsg := ""
	if task.LastError != nil {
		errMsg = task.LastError.Error()
	}
	w.AlertSink.Alert(task.Identifier, string(task.Source), notAfter, errMsg)
}
