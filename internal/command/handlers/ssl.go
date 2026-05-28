// SSL command handler — triggers ACME issuance for a domain or IP.
//
// Command shape (V4-DESIGN §8.2):
//
//	/w ssl  <domain-or-ip>  -          Request LE cert (default action)
//	/w ssl  <domain-or-ip>  selfsign   Force self-signed (rare; usually for testing)
//	/d ssl  <domain-or-ip>  -          Remove all certs for this identifier
//	/v ssl  <identifier>    -          Show cert status for one identifier
//	/v ssl  -               -          List all certs
//
// Note on responsibility (V4-DESIGN §3.2):
//
//	In single-node mode (steps 1-3), this node always performs issuance.
//	In multi-node mode (step 4+), only the responsible node (per hash
//	distribution over the candidate set) actually invokes ACME; other
//	candidate nodes share the challenge token via mesh and write their
//	challenge dirs accordingly.
//
//	In step 3, we simplify by always issuing locally. The upgrade path to
//	step 4 is: this handler checks "am I the responsible node?" and if not,
//	exits as no-op (which is correct per V4-DESIGN §2.2 — "no-op" and
//	"success" are equivalent in protocol terms).
package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/meshcdn/internal/cert"
	"github.com/example/meshcdn/internal/cert/acme"
	"github.com/example/meshcdn/internal/command"
)

// SSLHandler implements command.Handler for the "ssl" type.
//
// External wiring (set at executor construction time):
//   - CertStore: where to add the issued cert
//   - ACMEClient: ACME client to use (lazily constructed; nil means no LE)
//   - ChallengeDir: shared HTTP-01 challenge directory
//   - LocalNodeIP: this node's public IP (used to detect "should I route?")
//   - PeerIPs: function returning all known peer IPs (cluster member set)
//   - DelegateExec: function to remotely execute a command on another peer
//     (typically wired to mesh.Client.Exec). Returns the peer's output.
//   - ChallengeBroadcast: function to copy a challenge token to remote peers
//     and clean up afterwards. Wired to mesh event sender.
//   - CertBroadcast: function to push a freshly-issued domain cert to all
//     peers. Wired to mesh event sender. Optional — IP certs skip this.
type SSLHandler struct {
	CertStore    *cert.Store
	ACMEClient   *acme.Client
	ChallengeDir string

	// Step 10 routing wiring.
	LocalNodeIP        string
	PeerIPs            func() ([]string, error)
	DelegateExec       func(ctx context.Context, peerIP, command string) (output string, ok bool, err error)
	ChallengeBroadcast func(ctx context.Context, peerIPs []string, token, keyAuth string) error
	ChallengeCleanup   func(ctx context.Context, peerIPs []string, token string)
	CertBroadcast      func(ctx context.Context, certPEM, keyPEM []byte, source string)
}

func (h *SSLHandler) Type() string { return "ssl" }

func (h *SSLHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return scope, nil
}

func (h *SSLHandler) Validate(cmd *command.Command) error {
	switch cmd.Verb {
	case command.VerbWrite:
		if command.IsPlaceholder(cmd.Scope) {
			return command.NewError(command.ErrBadFormat,
				"/w ssl requires a domain or IP scope")
		}
		if !isValidIdentifier(cmd.Scope) {
			return command.NewError(command.ErrBadFormat,
				fmt.Sprintf("invalid identifier %q (must be domain or IP)", cmd.Scope))
		}
		// params: "-" (default = LE) or "selfsign"
		if !command.IsPlaceholder(cmd.Params) && cmd.Params != "selfsign" {
			return command.NewError(command.ErrBadParams,
				fmt.Sprintf("/w ssl params must be - or selfsign, got %q", cmd.Params))
		}
		return nil

	case command.VerbDelete:
		if command.IsPlaceholder(cmd.Scope) {
			return command.NewError(command.ErrBadFormat,
				"/d ssl requires a scope")
		}
		if !isValidIdentifier(cmd.Scope) {
			return command.NewError(command.ErrBadFormat,
				fmt.Sprintf("invalid identifier %q", cmd.Scope))
		}
		return nil

	case command.VerbView:
		// scope can be "-" (list all) or a specific identifier
		if !command.IsPlaceholder(cmd.Scope) && !isValidIdentifier(cmd.Scope) {
			return command.NewError(command.ErrBadFormat,
				fmt.Sprintf("invalid identifier %q", cmd.Scope))
		}
		return nil
	}
	return command.NewError(command.ErrBadFormat, "unknown verb")
}

// Write triggers ACME issuance (or self-sign if params=selfsign).
//
// Write issues an SSL cert. The full flow (per V4 step 10 design):
//
//  1. selfsign → local generate, no networking (fast path)
//
//  2. LE path:
//     a. Resolve identifier → candidate IPs (IP literal: itself; domain: DNS A records)
//     b. Intersect candidates with cluster peers → matched
//     - Empty matched: error "DNS doesn't point to any cluster node"
//     c. If local node is in matched: prepare challenge directory locally;
//     also broadcast challenge to other matched nodes (so LE can hit any
//     of them); then run ACME locally; cleanup.
//     d. If local node NOT in matched: pick a matched node and delegate
//     the entire /w ssl command to it via mesh /mesh/exec.
//     e. After issuance: domain cert → broadcast to all peers; IP cert →
//     keep local only (IPs are node-specific).
func (h *SSLHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	identifier := cmd.Scope

	if cmd.Params == "selfsign" {
		if h.CertStore == nil {
			return command.Effects{}, command.NewError(command.ErrInternal, "cert store not configured")
		}
		certPEM, keyPEM, err := cert.GenerateSelfSigned(identifier)
		if err != nil {
			return command.Effects{}, fmt.Errorf("generate self-signed: %w", err)
		}
		meta, err := h.CertStore.Add(certPEM, keyPEM, cert.SourceSelf)
		if err != nil {
			return command.Effects{}, fmt.Errorf("add to store: %w", err)
		}
		return command.Effects{
			NeedsNginxReload:  true,
			NeedsCertReselect: []string{identifier},
			UserMessage: fmt.Sprintf("自签证书已发放: %s (fingerprint=%s, expires=%s)",
				identifier, meta.FingerprintPrefix, meta.NotAfter.Format("2006-01-02")),
		}, nil
	}

	// LE path
	if h.ACMEClient == nil {
		return command.Effects{}, command.NewError(command.ErrInternal,
			"ACME client not configured")
	}

	// Step 1: candidate discovery (DNS resolution).
	candidates, err := resolveCandidateIPs(identifier)
	if err != nil {
		return command.Effects{}, fmt.Errorf("resolve %s: %w", identifier, err)
	}
	if len(candidates) == 0 {
		return command.Effects{}, command.NewError(command.ErrBadParams,
			fmt.Sprintf("无法解析 %s (DNS 没有返回任何 A 记录)", identifier))
	}

	// Step 2: intersect with cluster peers.
	peerIPs := []string{h.LocalNodeIP}
	if h.PeerIPs != nil {
		all, err := h.PeerIPs()
		if err == nil {
			peerIPs = all
		}
	}
	peerSet := make(map[string]bool)
	for _, p := range peerIPs {
		peerSet[p] = true
	}
	matched := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if peerSet[c] {
			matched = append(matched, c)
		}
	}
	if len(matched) == 0 {
		return command.Effects{}, command.NewError(command.ErrBadParams,
			fmt.Sprintf("%s 解析到 %v, 但这些 IP 都不在集群中。请先把 DNS 指向某个集群节点。",
				identifier, candidates))
	}

	// Step 3: routing decision.
	localInMatched := false
	for _, ip := range matched {
		if ip == h.LocalNodeIP {
			localInMatched = true
			break
		}
	}

	if !localInMatched {
		// Delegate to a matched peer.
		if h.DelegateExec == nil {
			return command.Effects{}, command.NewError(command.ErrInternal,
				"DelegateExec not wired (mesh disabled?)")
		}
		target := matched[0]
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		output, ok, err := h.DelegateExec(ctx, target, fmt.Sprintf("/w ssl %s -", identifier))
		if err != nil {
			return command.Effects{}, fmt.Errorf("delegate to %s: %w", target, err)
		}
		if !ok {
			return command.Effects{}, fmt.Errorf("delegate to %s reported failure: %s", target, output)
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("已委托节点 %s 申请 %s 证书:\n%s", target, identifier, output),
		}, nil
	}

	// Local execution path.
	// Step 4a: prepare challenge on other matched nodes (so LE can hit any).
	otherMatched := make([]string, 0, len(matched)-1)
	for _, ip := range matched {
		if ip != h.LocalNodeIP {
			otherMatched = append(otherMatched, ip)
		}
	}

	// Multi-peer challenge orchestration via ACME hooks.
	// When ACME tells us a challenge token, we:
	//   1. Write it locally (provider does this automatically)
	//   2. Push it to every other matched peer (so LE's HTTP-01 request
	//      can land on any peer and find the token)
	// On cleanup, we ask peers to remove their copy.
	//
	// If broadcast/cleanup hooks aren't wired (single-node mode), we skip
	// the multi-peer step. LE will still validate via the local node.
	var presentHook, cleanupHook func(domain, token, keyAuth string) error
	if len(otherMatched) > 0 && h.ChallengeBroadcast != nil {
		presentHook = func(domain, token, keyAuth string) error {
			cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return h.ChallengeBroadcast(cctx, otherMatched, token, keyAuth)
		}
	}
	if len(otherMatched) > 0 && h.ChallengeCleanup != nil {
		cleanupHook = func(domain, token, keyAuth string) error {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h.ChallengeCleanup(cctx, otherMatched, token)
			return nil
		}
	}

	certPEM, keyPEM, err := h.ACMEClient.IssueWithHooks(
		context.Background(), identifier, presentHook, cleanupHook)
	if err != nil {
		return command.Effects{}, fmt.Errorf("LE 申请失败: %w", err)
	}
	meta, err := h.CertStore.Add(certPEM, keyPEM, cert.SourceLE)
	if err != nil {
		return command.Effects{}, fmt.Errorf("add to store: %w", err)
	}

	// Step 5: domain certs broadcast cluster-wide; IP certs stay local.
	isIP := net.ParseIP(identifier) != nil
	if !isIP && h.CertBroadcast != nil {
		go h.CertBroadcast(context.Background(), certPEM, keyPEM, string(cert.SourceLE))
	}

	syncNote := ""
	if !isIP && h.CertBroadcast != nil {
		syncNote = " (已广播到全集群)"
	}
	if len(otherMatched) > 0 {
		syncNote += fmt.Sprintf(" [challenge 共享: %d 个候选节点]", len(otherMatched)+1)
	}

	return command.Effects{
		NeedsNginxReload:  true,
		NeedsCertReselect: []string{identifier},
		UserMessage: fmt.Sprintf("LE 证书已签发: %s (fingerprint=%s, expires=%s)%s",
			identifier, meta.FingerprintPrefix, meta.NotAfter.Format("2006-01-02"), syncNote),
	}, nil
}

// Delete removes ALL certs covering the given identifier.
// Mirror-symmetric: params is ignored.
func (h *SSLHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	identifier := cmd.Scope
	if h.CertStore == nil {
		return command.Effects{}, command.NewError(command.ErrInternal, "cert store not configured")
	}

	all, err := h.CertStore.All()
	if err != nil {
		return command.Effects{}, err
	}
	removed := 0
	for _, cm := range all {
		if cert.Covers(&cm, cert.Endpoint(identifier)) {
			if err := h.CertStore.Remove(cm.FingerprintPrefix); err != nil {
				return command.Effects{}, fmt.Errorf("remove %s: %w", cm.FingerprintPrefix, err)
			}
			removed++
		}
	}
	if removed == 0 {
		return command.Effects{
			UserMessage: fmt.Sprintf("未找到覆盖 %s 的证书", identifier),
		}, nil
	}
	return command.Effects{
		NeedsNginxReload:  true,
		NeedsCertReselect: []string{identifier},
		UserMessage:       fmt.Sprintf("已删除 %d 张覆盖 %s 的证书", removed, identifier),
	}, nil
}

// View returns cert status. /v ssl <id> -  → details for that identifier.
// /v ssl - -  → all certs with summary.
func (h *SSLHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	if h.CertStore == nil {
		return command.Effects{}, command.NewError(command.ErrInternal, "cert store not configured")
	}
	all, err := h.CertStore.All()
	if err != nil {
		return command.Effects{}, err
	}

	if command.IsPlaceholder(cmd.Scope) {
		// List all
		var sb strings.Builder
		fmt.Fprintf(&sb, "证书清单 (%d 张):\n", len(all))
		for _, cm := range all {
			sansStr := strings.Join(cm.SAN, ", ")
			fmt.Fprintf(&sb, "  [%s] %s | %s | not_after=%s | SAN: %s\n",
				cm.Source, cm.Subject, cm.FingerprintPrefix,
				cm.NotAfter.Format("2006-01-02"), sansStr)
		}
		if len(all) == 0 {
			sb.WriteString("  (无证书)\n")
		}
		return command.Effects{UserMessage: sb.String()}, nil
	}

	// Filter to those covering scope
	var matching []cert.CertMeta
	for _, cm := range all {
		if cert.Covers(&cm, cert.Endpoint(cmd.Scope)) {
			matching = append(matching, cm)
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "覆盖 %s 的证书 (%d 张):\n", cmd.Scope, len(matching))
	for _, cm := range matching {
		fmt.Fprintf(&sb, "  [%s] fingerprint=%s | issuer=%s | not_after=%s\n",
			cm.Source, cm.FingerprintPrefix, cm.Issuer,
			cm.NotAfter.Format("2006-01-02"))
	}
	if len(matching) == 0 {
		fmt.Fprintf(&sb, "  (无)\n")
	}
	return command.Effects{UserMessage: sb.String()}, nil
}

// isValidIdentifier returns true if s looks like a valid hostname or IP literal.
// Used by Validate. Accepts:
//   - IPs (net.ParseIP)
//   - simple hostnames (letters, digits, hyphens, dots)
func isValidIdentifier(s string) bool {
	if s == "" {
		return false
	}
	if net.ParseIP(s) != nil {
		return true
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '.':
			// ok
		default:
			return false
		}
	}
	return !strings.HasPrefix(s, ".") && !strings.HasSuffix(s, ".")
}

// AccountKeyPath returns the standard ACME account key location.
// Lives under persistent/ so it survives upgrades (it's effectively the
// node's identity to LE).
func AccountKeyPath() string {
	if v := os.Getenv("MESHCDN_ACME_ACCOUNT_KEY"); v != "" {
		return v
	}
	return filepath.Join("/etc/meshcdn/persistent", "acme-account.key")
}

// resolveCandidateIPs returns the IP set that LE might query for HTTP-01
// validation of identifier.
//
//   - If identifier is an IP literal: returns just that IP.
//   - If it's a hostname: does a DNS A-record lookup (default Go resolver,
//     reads /etc/resolv.conf or platform-equivalent). Returns all A records.
//
// IPv6/AAAA records are intentionally omitted — V4 only supports v4 mesh
// at this time, and LE prefers v4 for HTTP-01 anyway.
func resolveCandidateIPs(identifier string) ([]string, error) {
	if ip := net.ParseIP(identifier); ip != nil {
		return []string{ip.String()}, nil
	}
	addrs, err := net.LookupHost(identifier)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup: %w", err)
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		// Skip IPv6 — keep v4-only for now.
		if ip.To4() == nil {
			continue
		}
		out = append(out, ip.String())
	}
	return out, nil
}
