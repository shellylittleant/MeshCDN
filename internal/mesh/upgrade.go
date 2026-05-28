// Mesh upgrade support — added to mesh package for step 5.
//
// Adds:
//
//	GET  /mesh/binary       Returns the running cdn-agent binary
//	POST /mesh/upgrade      Initiates upgrade on receiver (download + replace + restart)
//
// Per V4-DESIGN §1.5: bandwidth probe + best-peer selection. Step 5 keeps
// it simple: every peer downloads from whoever sent /mesh/upgrade.
//
// Restart strategy (per V4-DESIGN §3.8 philosophy "let install be the source
// of truth"):
//   - Receiver writes new binary to <bin>.new
//   - Verifies sha256 (provided in /mesh/upgrade payload)
//   - Atomically renames to <bin>
//   - Triggers `systemctl restart meshcdn-agent.service` via detached subprocess
//
// The initiator does NOT wait for peers to actually restart. Each peer
// self-restarts; the cluster reconverges via heartbeat.
package mesh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"
)

type UpgradeRequest struct {
	FromIP       string `json:"from_ip"`
	BinarySHA256 string `json:"binary_sha256"`
	ProgramVer   string `json:"program_version"`
	BinarySize   int64  `json:"binary_size"`
}

type UpgradeResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// AddUpgradeRoutes registers /mesh/binary and /mesh/upgrade.
//
// Called from Server.Start additions; we don't auto-register because the
// caller must provide the binary path.
func (s *Server) AddUpgradeRoutes(binaryPath string, mux *http.ServeMux) {
	mux.HandleFunc("/mesh/binary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireAuth(w, r) {
			return
		}
		f, err := os.Open(binaryPath)
		if err != nil {
			http.Error(w, "binary not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
		_, _ = io.Copy(w, f)
	})

	mux.HandleFunc("/mesh/upgrade", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireAuth(w, r) {
			return
		}
		var req UpgradeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Acknowledge first; perform upgrade asynchronously.
		writeJSON(w, http.StatusOK, &UpgradeResponse{OK: true})
		go func() {
			if err := s.performLocalUpgrade(req, binaryPath); err != nil {
				log.Printf("[mesh/upgrade] failed: %v", err)
			}
		}()
	})
}

// performLocalUpgrade downloads, verifies, replaces, and triggers restart.
func (s *Server) performLocalUpgrade(req UpgradeRequest, binaryPath string) error {
	log.Printf("[mesh/upgrade] starting from %s, expected sha=%s, size=%d",
		req.FromIP, req.BinarySHA256, req.BinarySize)

	// Use a one-off HTTP client; this is independent of the agent's main client.
	client := NewClient(s.Token)
	hc := client.http()
	hc.Timeout = 5 * time.Minute

	url := peerURL(req.FromIP, s.Port, "/mesh/binary")
	httpReq, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set(AuthHeader, "Bearer "+s.Token)

	resp, err := hc.Do(httpReq)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	tmpPath := binaryPath + ".new"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}

	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)
	written, err := io.Copy(mw, resp.Body)
	_ = f.Close()
	if err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy: %w", err)
	}
	if req.BinarySize > 0 && written != req.BinarySize {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("size mismatch: got %d, expected %d", written, req.BinarySize)
	}
	gotSHA := hex.EncodeToString(hasher.Sum(nil))
	if gotSHA != req.BinarySHA256 {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sha256 mismatch: got %s, expected %s", gotSHA, req.BinarySHA256)
	}

	if err := os.Rename(tmpPath, binaryPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	log.Printf("[mesh/upgrade] binary replaced (%d bytes); restarting in 0.5s", written)

	// Detached restart so it survives our own death.
	go func() {
		time.Sleep(500 * time.Millisecond)
		cmd := exec.Command("systemctl", "restart", "meshcdn-agent.service")
		_ = cmd.Run()
	}()
	return nil
}

// TriggerUpgradeForAll sends /mesh/upgrade to every peer in parallel.
//
// initiatorIP is the IP peers will download from (this node).
// binaryPath is read locally to compute sha256 and size.
func (c *Client) TriggerUpgradeForAll(
	ctx context.Context,
	peerIPs []string,
	port int,
	initiatorIP string,
	binaryPath string,
	programVer string,
) error {
	f, err := os.Open(binaryPath)
	if err != nil {
		return fmt.Errorf("open binary: %w", err)
	}
	defer f.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, f)
	if err != nil {
		return fmt.Errorf("hash binary: %w", err)
	}
	sha := hex.EncodeToString(hasher.Sum(nil))

	req := &UpgradeRequest{
		FromIP:       initiatorIP,
		BinarySHA256: sha,
		ProgramVer:   programVer,
		BinarySize:   size,
	}

	failures := 0
	for _, peerIP := range peerIPs {
		if peerIP == initiatorIP {
			continue
		}
		var resp UpgradeResponse
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.postJSON(callCtx, peerURL(peerIP, port, "/mesh/upgrade"), req, &resp)
		cancel()
		if err != nil {
			log.Printf("[upgrade] peer %s: %v", peerIP, err)
			failures++
			continue
		}
		if !resp.OK {
			log.Printf("[upgrade] peer %s: rejected: %s", peerIP, resp.Reason)
			failures++
		}
	}

	if failures > 0 {
		return fmt.Errorf("upgrade triggered with %d failures", failures)
	}
	return nil
}
