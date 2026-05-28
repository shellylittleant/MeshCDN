// Mesh client — outbound HTTP calls to peers.
//
// Each Client targets ALL known peers; choice of which peer to call for
// a given operation is the caller's responsibility (e.g. heartbeat calls
// every peer; snapshot pull picks the best peer based on RTT).
//
// TLS: clients use InsecureSkipVerify because each peer presents its own
// self-signed cert. This is intentional per V4-DESIGN §A.1 — auth carries
// in the bearer token, TLS only encrypts.
package mesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client makes outbound mesh calls.
type Client struct {
	// Token is the bearer token to present (sha256(group_id+bot_token)).
	Token string

	// Timeout for individual requests. Heartbeat uses short timeout (5s);
	// snapshot pull tolerates longer (30s).
	Timeout time.Duration

	// Underlying HTTP client. Lazily initialized on first call.
	httpClient *http.Client
}

// NewClient returns a Client with sensible defaults.
func NewClient(token string) *Client {
	return &Client{
		Token:   token,
		Timeout: 10 * time.Second,
	}
}

func (c *Client) http() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	c.httpClient = &http.Client{
		Timeout: c.Timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // intentional: self-signed certs everywhere
				MinVersion:         tls.VersionTLS12,
			},
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
		},
	}
	return c.httpClient
}

// peerURL builds "https://<peerIP>:<port><path>".
func peerURL(peerIP string, port int, path string) string {
	if port == 0 {
		port = DefaultPort
	}
	return fmt.Sprintf("https://%s:%d%s", peerIP, port, path)
}

// ─────────────────────────────────────────────────────────────────────
// Handshake (called by a new node joining)
// ─────────────────────────────────────────────────────────────────────

// Handshake sends our auth proof to the introducer and receives the cluster
// snapshot + peer list.
func (c *Client) Handshake(ctx context.Context, peerIP string, port int, req *HandshakeRequest) (*HandshakeResponse, error) {
	var resp HandshakeResponse
	if err := c.postJSON(ctx, peerURL(peerIP, port, "/mesh/auth"), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ─────────────────────────────────────────────────────────────────────
// Heartbeat
// ─────────────────────────────────────────────────────────────────────

// Ping sends a heartbeat to a peer. Returns the peer's reported state.
// Caller measures RTT around this call to track peer responsiveness.
func (c *Client) Ping(ctx context.Context, peerIP string, port int, req *PingRequest) (*PingResponse, error) {
	var resp PingResponse
	if err := c.postJSON(ctx, peerURL(peerIP, port, "/mesh/ping"), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ─────────────────────────────────────────────────────────────────────
// Snapshot pull
// ─────────────────────────────────────────────────────────────────────

// SnapshotPull fetches the snapshot text from a peer. Used by lagging nodes
// to catch up to the cluster's current configuration.
//
// Uses its own HTTP client with longer timeout (30s) since snapshots can
// be larger than ping/notify payloads.
func (c *Client) SnapshotPull(ctx context.Context, peerIP string, port int) (string, error) {
	// Build a dedicated client for this call (longer timeout).
	// We can't shallow-copy *http.Client safely (it contains internal state).
	pullClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS12,
			},
		},
	}

	url := peerURL(peerIP, port, "/mesh/snapshot-pull")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(AuthHeader, "Bearer "+c.Token)

	r, err := pullClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("snapshot pull from %s: %w", peerIP, err)
	}
	defer r.Body.Close()

	if r.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
		return "", fmt.Errorf("snapshot pull from %s: HTTP %d: %s", peerIP, r.StatusCode, string(body))
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", fmt.Errorf("snapshot pull body: %w", err)
	}
	return string(body), nil
}

// ─────────────────────────────────────────────────────────────────────
// Notify version
// ─────────────────────────────────────────────────────────────────────

// NotifyVersion tells a peer "I have version V available". The peer compares
// to its local version and pulls if behind.
func (c *Client) NotifyVersion(ctx context.Context, peerIP string, port int, req *NotifyVersionRequest) error {
	var resp NotifyVersionResponse
	if err := c.postJSON(ctx, peerURL(peerIP, port, "/mesh/notify-version"), req, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("peer rejected notify")
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────
// Event broadcast
// ─────────────────────────────────────────────────────────────────────

// Event sends an event-stream message to a peer (alerts, challenge-share, etc).
// Per V4-DESIGN §1.6, events are best-effort: failures don't bubble up.
func (c *Client) Event(ctx context.Context, peerIP string, port int, msg *EventMessage) error {
	var resp map[string]bool
	return c.postJSON(ctx, peerURL(peerIP, port, "/mesh/event"), msg, &resp)
}

// PostEvent is a convenience wrapper that constructs the EventMessage and
// uses default mesh port. Used by cluster event broadcasters where the
// caller doesn't want to pass port + FromIP repeatedly.
//
// fromIP is captured from the closure context (set up at wiring time).
type postEventFunc = func(ctx context.Context, peerIP, eventType string, payload map[string]interface{}) error

// MakePostEvent returns a closure suitable for the bot package's
// Broadcast{Challenge,Cert} factories. fromIP is "this node's IP".
func (c *Client) MakePostEvent(fromIP string, port int) postEventFunc {
	if port == 0 {
		port = DefaultPort
	}
	return func(ctx context.Context, peerIP, eventType string, payload map[string]interface{}) error {
		msg := &EventMessage{
			FromIP:  fromIP,
			Type:    eventType,
			Payload: payload,
		}
		return c.Event(ctx, peerIP, port, msg)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Remote command execution
// ─────────────────────────────────────────────────────────────────────

// Exec sends a command to a remote peer for execution and returns the
// peer's executor output. Used by the bot node to delegate /w ssl <id>
// to the node that actually owns the IP / domain (so HTTP-01 challenges
// land on the right machine).
//
// Uses its own long-timeout HTTP client because LE issuance (DNS lookup +
// ACME order + HTTP-01 validation + cert download) takes 10-30 seconds —
// far longer than the default 10s mesh timeout. ctx still controls the
// outer deadline; this client just removes the http.Client.Timeout cap.
func (c *Client) Exec(ctx context.Context, peerIP string, port int, req *ExecRequest) (*ExecResponse, error) {
	execClient := &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS12,
			},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal exec req: %w", err)
	}
	url := peerURL(peerIP, port, "/mesh/exec")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(AuthHeader, "Bearer "+c.Token)

	r, err := execClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", url, err)
	}
	defer r.Body.Close()

	if r.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
		return nil, fmt.Errorf("call %s: HTTP %d: %s", url, r.StatusCode, string(errBody))
	}

	var resp ExecResponse
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode exec resp: %w", err)
	}
	return &resp, nil
}

// ─────────────────────────────────────────────────────────────────────
// Internal: postJSON helper
// ─────────────────────────────────────────────────────────────────────

func (c *Client) postJSON(ctx context.Context, url string, reqBody, respBody interface{}) error {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(AuthHeader, "Bearer "+c.Token)

	r, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", url, err)
	}
	defer r.Body.Close()

	if r.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
		return fmt.Errorf("call %s: HTTP %d: %s", url, r.StatusCode, string(errBody))
	}

	if respBody != nil {
		if err := json.NewDecoder(r.Body).Decode(respBody); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
