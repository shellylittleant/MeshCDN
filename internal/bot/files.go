// Bot file upload helpers.
//
// Telegram lets us attach files to messages with a caption like
// "/w sslfile a.com -". Cert uploads need TWO files (.crt and .key); user
// sends them as separate messages. We buffer one half until the other
// arrives, then invoke the command with both PEMs in environment vars.
package bot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-telegram/bot"

	"github.com/example/meshcdn/internal/command"
)

// uploadBuffers is the in-memory store of half-completed cert uploads.
//
// Per V4-DESIGN philosophy: state is process-bound, no persistence. If the
// agent restarts mid-upload, user re-sends both files. 5-minute expiry.
type uploadBuffers struct {
	mu sync.Mutex
	m  map[int64]*uploadEntry
}

type uploadEntry struct {
	cert      string // PEM text
	key       string
	caption   string // the original "/w sslfile a.com -" command
	createdAt time.Time
}

// uploadBuffers is a method receiver target: c.uploadBuffers.put / .get / .clear.
// We expose it as a field on Client (defined below).

// put stores one half of a cert upload. slot must be "cert" or "key".
func (u *uploadBuffers) put(userID int64, slot, content, caption string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.m == nil {
		u.m = make(map[int64]*uploadEntry)
	}
	e, ok := u.m[userID]
	if !ok {
		e = &uploadEntry{createdAt: time.Now()}
		u.m[userID] = e
	}
	switch slot {
	case "cert":
		e.cert = content
	case "key":
		e.key = content
	}
	e.caption = caption
	e.createdAt = time.Now() // refresh
}

// get returns the cert/key/caption for a user's buffer.
func (u *uploadBuffers) get(userID int64) (cert, key, caption string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	e, ok := u.m[userID]
	if !ok {
		return "", "", ""
	}
	if time.Since(e.createdAt) > 5*time.Minute {
		delete(u.m, userID)
		return "", "", ""
	}
	return e.cert, e.key, e.caption
}

// clear removes a user's buffer (after successful upload).
func (u *uploadBuffers) clear(userID int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.m, userID)
}

// sweep removes expired entries; called periodically from serve.go.
func (u *uploadBuffers) sweep() {
	u.mu.Lock()
	defer u.mu.Unlock()
	cutoff := time.Now().Add(-5 * time.Minute)
	for k, e := range u.m {
		if e.createdAt.Before(cutoff) {
			delete(u.m, k)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────
// FileFetcher implementation using Telegram's file API
// ─────────────────────────────────────────────────────────────────────

// TelegramFileFetcher implements FileFetcher by calling bot.GetFile then
// downloading the resulting file URL.
type TelegramFileFetcher struct {
	Bot   *bot.Bot
	Token string
}

func (f *TelegramFileFetcher) Fetch(ctx context.Context, fileID string) ([]byte, error) {
	if f.Bot == nil {
		return nil, fmt.Errorf("bot not initialized")
	}
	file, err := f.Bot.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}
	if file.FilePath == "" {
		return nil, fmt.Errorf("Telegram returned empty file path")
	}
	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", f.Token, file.FilePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	const maxFile = 5 * 1024 * 1024 // 5MB cap (cert PEMs are <10KB; defensive)
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFile))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}

// ─────────────────────────────────────────────────────────────────────
// Upload command execution
// ─────────────────────────────────────────────────────────────────────

// executeWithUploadEnv sets MESHCDN_UPLOAD_CERT_PEM / MESHCDN_UPLOAD_KEY_PEM
// in the process environment, runs the executor on caption, then unsets.
//
// Per V4-DESIGN §8.2: upload PEMs go via env vars (CLI-friendly path).
// In-process state mutation is safe here because the bot dispatches messages
// serially within a single goroutine via the bot library; concurrent uploads
// from different users would still need the env var pattern OR a thread-safe
// alternative. Step 7 takes the simple path; if you serve many concurrent
// upload users, refactor to pass PEMs through a context value or a registry
// keyed on a request ID.
func executeWithUploadEnv(ctx context.Context, exec *command.Executor,
	cmdText, certPEM, keyPEM string) error {

	prevCert := os.Getenv("MESHCDN_UPLOAD_CERT_PEM")
	prevKey := os.Getenv("MESHCDN_UPLOAD_KEY_PEM")
	defer func() {
		_ = os.Setenv("MESHCDN_UPLOAD_CERT_PEM", prevCert)
		_ = os.Setenv("MESHCDN_UPLOAD_KEY_PEM", prevKey)
	}()
	if err := os.Setenv("MESHCDN_UPLOAD_CERT_PEM", certPEM); err != nil {
		return err
	}
	if err := os.Setenv("MESHCDN_UPLOAD_KEY_PEM", keyPEM); err != nil {
		return err
	}

	result, err := exec.ExecuteBatch(ctx, cmdText)
	if err != nil {
		return err
	}
	if result.AnyFailed() {
		for _, o := range result.Outcomes {
			if o.Err != nil {
				return o.Err
			}
		}
	}
	return nil
}
