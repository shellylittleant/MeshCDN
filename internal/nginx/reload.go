// Nginx reload mechanism.
//
// The agent doesn't manage nginx as a process — that's systemd's job.
// We just signal reload via `nginx -s reload` (SIGHUP equivalent).
//
// Before reload, we run `nginx -t` to validate the config. If validation
// fails, we don't attempt reload — the user gets a clear error. This
// matches the V4-DESIGN philosophy "程序的边界 = 信息边界": we surface
// the problem; we don't try to fix or work around it.
package nginx

import (
	"fmt"
	"os/exec"
	"strings"
)

// NginxBinary is the path to the OpenResty nginx binary.
// Override via $MESHCDN_NGINX_BINARY for non-standard installs.
var NginxBinary = "/usr/local/openresty/nginx/sbin/nginx"

// Validate runs `nginx -t -c <main-conf>` to check the config syntax.
// Returns nil if config is valid; otherwise an error containing nginx's
// stderr output.
func Validate(mainConfPath string) error {
	cmd := exec.Command(NginxBinary, "-t", "-c", mainConfPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nginx -t failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Reload signals nginx to gracefully reload config (`nginx -s reload`).
// Should be called only after Validate succeeds.
//
// Returns an error containing nginx's stderr if reload fails.
func Reload(mainConfPath string) error {
	cmd := exec.Command(NginxBinary, "-s", "reload", "-c", mainConfPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nginx -s reload failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ValidateAndReload runs validation followed by reload. This is the
// single entry point callers should use after regenerating config.
func ValidateAndReload(mainConfPath string) error {
	if err := Validate(mainConfPath); err != nil {
		return fmt.Errorf("config validation failed (reload skipped): %w", err)
	}
	return Reload(mainConfPath)
}

// IsRunning checks whether nginx is currently running by attempting
// `nginx -s reopen` (a no-op if running, fails if not). Used at boot to
// distinguish "first start" (need to launch via systemd) from "already up".
//
// This is a heuristic; the canonical way to check process state is via
// systemd / pid file. We keep it simple here.
func IsRunning(mainConfPath string) bool {
	cmd := exec.Command(NginxBinary, "-s", "reopen", "-c", mainConfPath)
	return cmd.Run() == nil
}
