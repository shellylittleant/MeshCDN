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
	"strings"
	"time"
)

// agentService is the systemd unit restarted/rolled-back during an upgrade.
const agentService = "meshcdn-agent.service"

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

	// Back up the currently-installed binary BEFORE overwriting it, so the
	// watchdog (and a human) can roll back if the new one won't come up.
	backupPath := binaryPath + ".old"
	if err := copyFile(binaryPath, backupPath); err != nil {
		log.Printf("[mesh/upgrade] warning: could not back up current binary: %v (rollback disabled)", err)
		backupPath = ""
	}

	if err := os.Rename(tmpPath, binaryPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	log.Printf("[mesh/upgrade] binary replaced (%d bytes); launching restart+health watchdog", written)
	launchRestartWatchdog(binaryPath, backupPath)
	return nil
}

// launchRestartWatchdog restarts the agent and, if the new binary fails to
// become healthy within ~60s, rolls back to backupPath and restarts again.
//
// It must run OUTSIDE our own systemd cgroup: the unit is KillMode=mixed, so a
// child we fork would be SIGKILLed the moment `systemctl restart` tears our
// cgroup down. `systemd-run` launches the watchdog as an independent transient
// unit that survives. If systemd-run is unavailable we fall back to a plain
// detached restart with no rollback (best effort).
func launchRestartWatchdog(binaryPath, backupPath string) {
	script := watchdogScript(binaryPath, backupPath)

	if _, err := exec.LookPath("systemd-run"); err == nil {
		cmd := exec.Command("systemd-run", "--collect",
			"--description=MeshCDN upgrade watchdog", "/bin/sh", "-c", script)
		if err := cmd.Run(); err == nil {
			return
		} else {
			log.Printf("[mesh/upgrade] systemd-run failed (%v); falling back to plain restart (no rollback)", err)
		}
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = exec.Command("systemctl", "restart", agentService).Run()
	}()
}

// watchdogScript builds the /bin/sh program the watchdog runs. Health = agent
// reports "active" for 3 consecutive checks (≈9s stable) within a ~60s window;
// a crash-looping binary (Restart=on-failure) reaches "failed" well inside that
// window, so it never accumulates 3 in a row and triggers rollback. When
// backupPath is empty (backup failed) the rollback tail is omitted.
func watchdogScript(binaryPath, backupPath string) string {
	rollback := ""
	if backupPath != "" {
		rollback = fmt.Sprintf(`
logger -t meshcdn-upgrade "new binary unhealthy — rolling back"
cp -f %[2]s %[1]s
systemctl restart %[3]s`, binaryPath, backupPath, agentService)
	}
	return fmt.Sprintf(`
systemctl restart %[1]s
stable=0
for i in $(seq 1 20); do
  if systemctl is-active --quiet %[1]s; then stable=$((stable+1)); else stable=0; fi
  if [ "$stable" -ge 3 ]; then logger -t meshcdn-upgrade "upgrade healthy"; exit 0; fi
  sleep 3
done%[2]s
`, agentService, rollback)
}

// copyFile copies src to dst (0755). Used to snapshot the current binary.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// verifyBudget is how long TriggerUpgradeForAll waits for peers to come back on
// the new version before reporting. Peers restart in seconds; the watchdog's
// rollback window is ~60s, so we poll a bit beyond that.
const verifyBudget = 90 * time.Second

// TriggerUpgradeForAll pushes the upgrade to every peer, then VERIFIES each one
// actually came back on the new version (polling heartbeats), and returns a
// per-node report. This is the fix for silent "fire-and-forget" failures: the
// operator sees exactly which nodes upgraded and which didn't.
//
// initiatorIP is the IP peers download from (this node); it is assumed to be
// already running programVer (operator deployed the new binary here first).
func (c *Client) TriggerUpgradeForAll(
	ctx context.Context,
	peerIPs []string,
	port int,
	initiatorIP string,
	binaryPath string,
	programVer string,
) (string, error) {
	f, err := os.Open(binaryPath)
	if err != nil {
		return "", fmt.Errorf("open binary: %w", err)
	}
	defer f.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, f)
	if err != nil {
		return "", fmt.Errorf("hash binary: %w", err)
	}
	sha := hex.EncodeToString(hasher.Sum(nil))

	req := &UpgradeRequest{
		FromIP:       initiatorIP,
		BinarySHA256: sha,
		ProgramVer:   programVer,
		BinarySize:   size,
	}

	// Phase 1: trigger. Record which peers accepted vs. rejected outright.
	var targets []string
	triggerFail := map[string]string{}
	for _, peerIP := range peerIPs {
		if peerIP == initiatorIP {
			continue
		}
		var resp UpgradeResponse
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.postJSON(callCtx, peerURL(peerIP, port, "/mesh/upgrade"), req, &resp)
		cancel()
		if err != nil {
			triggerFail[peerIP] = err.Error()
			continue
		}
		if !resp.OK {
			triggerFail[peerIP] = resp.Reason
			continue
		}
		targets = append(targets, peerIP)
	}

	// Phase 2: verify. Poll each accepted peer's heartbeat until it reports the
	// new program version, or the budget expires (they may be mid-restart).
	confirmed := map[string]bool{}
	deadline := time.Now().Add(verifyBudget)
	for len(confirmed) < len(targets) && time.Now().Before(deadline) {
		for _, ip := range targets {
			if confirmed[ip] {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			pr, perr := c.Ping(pctx, ip, port, &PingRequest{ProgramVersion: programVer})
			cancel()
			if perr == nil && pr != nil && pr.ProgramVersion == programVer {
				confirmed[ip] = true
			}
		}
		if len(confirmed) == len(targets) {
			break
		}
		select {
		case <-ctx.Done():
			return buildUpgradeReport(programVer, initiatorIP, targets, confirmed, triggerFail), ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}

	report := buildUpgradeReport(programVer, initiatorIP, targets, confirmed, triggerFail)
	if len(confirmed) < len(targets) || len(triggerFail) > 0 {
		return report, fmt.Errorf("upgrade incomplete: %d/%d peer 已确认", len(confirmed), len(targets)+len(triggerFail))
	}
	return report, nil
}

func buildUpgradeReport(programVer, initiatorIP string, targets []string, confirmed map[string]bool, triggerFail map[string]string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "🚀 集群升级 → %s\n", programVer)
	fmt.Fprintf(&sb, "  %s (发起节点): 已是 %s ✓\n", initiatorIP, programVer)
	ok := 0
	for _, ip := range targets {
		if confirmed[ip] {
			fmt.Fprintf(&sb, "  %s: 已升级到 %s ✓\n", ip, programVer)
			ok++
		} else {
			fmt.Fprintf(&sb, "  %s: 未确认 (重启中/已回滚? 用 /v nodes 复核) ✗\n", ip)
		}
	}
	for ip, reason := range triggerFail {
		fmt.Fprintf(&sb, "  %s: 触发失败: %s ✗\n", ip, reason)
	}
	total := len(targets) + len(triggerFail)
	fmt.Fprintf(&sb, "小结: %d/%d peer 已确认升级到 %s\n", ok, total, programVer)
	if ok < total {
		sb.WriteString("失败节点已保留旧二进制 <bin>.old 并会自动回滚；如仍异常请 SSH 检查 `journalctl -u meshcdn-agent`")
	}
	return sb.String()
}
