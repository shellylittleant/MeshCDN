// SSL file handler — for uploading user-provided certificates.
//
// Per V4-DESIGN §8.2:
//
//	/w sslfile  <domain-or-ip>  -                      Upload (cert+key from external source)
//	/w sslfile  <domain-or-ip>  use                    Upload + immediately switch to current
//	/v sslfile  <domain-or-ip>  <name>                 Download specific cert
//	/v sslfile  <domain-or-ip>  -                      Download current selected cert
//	/d sslfile  <domain-or-ip>  <name>                 Delete specific cert
//	/d sslfile  <domain-or-ip>  -                      Delete all uploaded certs for this id
//
// The actual upload mechanism depends on the front-end:
//   - Telegram bot: file attachment (step 7)
//   - CLI: read PEM bytes from env vars MESHCDN_UPLOAD_CERT_PEM and MESHCDN_UPLOAD_KEY_PEM
//     (step 3 default)
//
// When uploaded via CLI env vars, the user constructs the command thus:
//
//	MESHCDN_UPLOAD_CERT_PEM=$(cat my-cert.pem) \
//	MESHCDN_UPLOAD_KEY_PEM=$(cat my-key.pem) \
//	cdn-agent exec "/w sslfile a.com -"
//
// This isn't elegant, but it's the simplest cross-platform "upload" path
// that works without filesystem dependencies. Telegram bot will provide
// a much nicer UX in step 7.
package handlers

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/example/meshcdn/internal/cert"
	"github.com/example/meshcdn/internal/command"
)

// SSLFileHandler implements command.Handler for the "sslfile" type.
type SSLFileHandler struct {
	CertStore *cert.Store
}

func (h *SSLFileHandler) Type() string { return "sslfile" }

func (h *SSLFileHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return scope, nil
}

func (h *SSLFileHandler) Validate(cmd *command.Command) error {
	switch cmd.Verb {
	case command.VerbWrite:
		if command.IsPlaceholder(cmd.Scope) {
			return command.NewError(command.ErrBadFormat,
				"/w sslfile requires a domain or IP scope")
		}
		if !isValidIdentifier(cmd.Scope) {
			return command.NewError(command.ErrBadFormat,
				fmt.Sprintf("invalid identifier %q", cmd.Scope))
		}
		// params: "-" or "use"
		if !command.IsPlaceholder(cmd.Params) && cmd.Params != "use" {
			return command.NewError(command.ErrBadParams,
				"params must be - or 'use'")
		}
		return nil

	case command.VerbDelete:
		if command.IsPlaceholder(cmd.Scope) {
			return command.NewError(command.ErrBadFormat,
				"/d sslfile requires a scope")
		}
		return nil

	case command.VerbView:
		if command.IsPlaceholder(cmd.Scope) {
			return command.NewError(command.ErrBadFormat,
				"/v sslfile requires a scope")
		}
		return nil
	}
	return command.NewError(command.ErrBadFormat, "unknown verb")
}

// Write reads cert+key PEM from MESHCDN_UPLOAD_CERT_PEM and MESHCDN_UPLOAD_KEY_PEM
// env vars (CLI mode) and stores them.
//
// Telegram bot integration (step 7) will populate these vars before invoking
// the executor.
func (h *SSLFileHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	if h.CertStore == nil {
		return command.Effects{}, command.NewError(command.ErrInternal, "cert store not configured")
	}

	certPEMText := os.Getenv("MESHCDN_UPLOAD_CERT_PEM")
	keyPEMText := os.Getenv("MESHCDN_UPLOAD_KEY_PEM")
	if certPEMText == "" || keyPEMText == "" {
		return command.Effects{}, command.NewError(command.ErrBadParams,
			"need MESHCDN_UPLOAD_CERT_PEM and MESHCDN_UPLOAD_KEY_PEM env vars")
	}

	// Verify PEM parses
	parsed, err := cert.ParsePEM([]byte(certPEMText))
	if err != nil {
		return command.Effects{}, command.NewError(command.ErrBadParams,
			fmt.Sprintf("invalid cert PEM: %v", err))
	}

	// Optional: verify the cert covers the claimed scope. We check subject
	// and SAN; mismatch is a warning, not an error (user might have a wildcard
	// cert and want to upload it for a specific subdomain).
	covers := false
	if parsed.Subject.CommonName == cmd.Scope {
		covers = true
	}
	for _, san := range parsed.DNSNames {
		if san == cmd.Scope {
			covers = true
			break
		}
	}
	for _, ip := range parsed.IPAddresses {
		if ip.String() == cmd.Scope {
			covers = true
			break
		}
	}

	meta, err := h.CertStore.Add([]byte(certPEMText), []byte(keyPEMText), cert.SourceUpload)
	if err != nil {
		return command.Effects{}, fmt.Errorf("add to store: %w", err)
	}

	msg := fmt.Sprintf("上传证书已保存: subject=%s, fingerprint=%s, expires=%s",
		meta.Subject, meta.FingerprintPrefix, meta.NotAfter.Format("2006-01-02"))
	if !covers {
		msg += fmt.Sprintf("\n  ⚠️ 注意: 该证书未覆盖 %s (subject=%s, SAN=%v)",
			cmd.Scope, parsed.Subject.CommonName, parsed.DNSNames)
	}

	return command.Effects{
		NeedsNginxReload:  true,
		NeedsCertReselect: []string{cmd.Scope},
		UserMessage:       msg,
	}, nil
}

// Delete removes uploaded cert(s) for the given scope.
// Only certs with source="upload" are removed (LE/self left alone).
func (h *SSLFileHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	if h.CertStore == nil {
		return command.Effects{}, command.NewError(command.ErrInternal, "cert store not configured")
	}
	all, err := h.CertStore.All()
	if err != nil {
		return command.Effects{}, err
	}
	removed := 0
	for _, cm := range all {
		if cm.Source != cert.SourceUpload {
			continue
		}
		if !cert.Covers(&cm, cert.Endpoint(cmd.Scope)) {
			continue
		}
		// If params is a fingerprint prefix, only remove that one
		if !command.IsPlaceholder(cmd.Params) {
			if !strings.HasPrefix(cm.FingerprintPrefix, cmd.Params) {
				continue
			}
		}
		if err := h.CertStore.Remove(cm.FingerprintPrefix); err != nil {
			return command.Effects{}, err
		}
		removed++
	}
	if removed == 0 {
		return command.Effects{
			UserMessage: fmt.Sprintf("未找到匹配的上传证书 (%s)", cmd.Scope),
		}, nil
	}
	return command.Effects{
		NeedsNginxReload:  true,
		NeedsCertReselect: []string{cmd.Scope},
		UserMessage:       fmt.Sprintf("已删除 %d 张上传证书", removed),
	}, nil
}

// View — currently just echoes; actual file download via Telegram is step 7.
func (h *SSLFileHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	if h.CertStore == nil {
		return command.Effects{}, command.NewError(command.ErrInternal, "cert store not configured")
	}
	all, err := h.CertStore.All()
	if err != nil {
		return command.Effects{}, err
	}
	var matching []cert.CertMeta
	for _, cm := range all {
		if cert.Covers(&cm, cert.Endpoint(cmd.Scope)) {
			matching = append(matching, cm)
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "证书文件 (%s, %d 张):\n", cmd.Scope, len(matching))
	for _, cm := range matching {
		fmt.Fprintf(&sb, "  [%s] %s\n", cm.Source, cm.FingerprintPrefix)
	}
	if len(matching) == 0 {
		sb.WriteString("  (无)\n")
	}
	sb.WriteString("\n注: 实际文件下载需通过 Telegram bot (step 7)。\n")
	return command.Effects{UserMessage: sb.String()}, nil
}
