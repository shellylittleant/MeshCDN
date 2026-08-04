// Package cli — UPDATED for step 8 (passes IdentityPath for AI handler).
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/example/meshcdn/internal/cert"
	"github.com/example/meshcdn/internal/cert/acme"
	"github.com/example/meshcdn/internal/command"
	"github.com/example/meshcdn/internal/command/handlers"
	"github.com/example/meshcdn/internal/db"
	"github.com/example/meshcdn/internal/identity"
	"github.com/example/meshcdn/internal/logs"
	"github.com/example/meshcdn/internal/peers"
	"github.com/example/meshcdn/internal/snapshot"
	"github.com/example/meshcdn/internal/version"
)

var (
	DefaultDBPath       = "/etc/meshcdn/runtime/config.db"
	DefaultCertsDir     = "/etc/meshcdn/persistent/certs"
	DefaultNginxConfDir = "/etc/meshcdn/runtime/nginx"
	DefaultChallengeDir = "/etc/meshcdn/runtime/challenges"
)

func pathOverride(envVar, def string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return def
}

func Exec(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cdn-agent exec \"<command>\"")
	}

	dbPath := pathOverride("MESHCDN_DB_PATH", DefaultDBPath)
	certsDir := pathOverride("MESHCDN_CERTS_DIR", DefaultCertsDir)
	nginxDir := pathOverride("MESHCDN_NGINX_DIR", DefaultNginxConfDir)
	challengeDir := pathOverride("MESHCDN_CHALLENGE_DIR", DefaultChallengeDir)
	skipNginx := os.Getenv("MESHCDN_NO_NGINX") == "1"
	disableACME := os.Getenv("MESHCDN_ACME_DISABLE") == "1"

	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}

	conn, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer conn.Close()

	store, err := cert.NewStore(certsDir)
	if err != nil {
		return fmt.Errorf("open cert store: %w", err)
	}

	var acmeClient *acme.Client
	if !disableACME {
		accountKeyPath := filepath.Join("/etc/meshcdn/persistent", "acme-account.key")
		if v := os.Getenv("MESHCDN_ACME_ACCOUNT_KEY"); v != "" {
			accountKeyPath = v
		}
		acmeClient, err = acme.NewClient(accountKeyPath, "", challengeDir+"/.well-known/acme-challenge")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: ACME client init failed: %v\n", err)
			acmeClient = nil
		}
	}

	peerMgr, err := peers.NewManager(peers.Path())
	if err != nil {
		return fmt.Errorf("open peers.json: %w", err)
	}

	// Task 3: open logs.db read-side so `/v stats` works from the CLI too.
	// Best-effort — absence just means stats reports "未启用".
	logsDBPath := pathOverride("MESHCDN_LOGS_DB_PATH", "/etc/meshcdn/runtime/logs.db")
	var logStore *logs.Store
	if ls, lerr := logs.Open(logsDBPath); lerr == nil {
		logStore = ls
		defer logStore.Close()
	}

	nodeIP := loadNodeIPBestEffort()

	registry := handlers.NewRegistry(handlers.Dependencies{
		CertStore:      store,
		ACMEClient:     acmeClient,
		ChallengeDir:   challengeDir + "/.well-known/acme-challenge",
		DB:             conn,
		ProgramVersion: version.Version,
		PeerMgr:        peerMgr,
		LocalNodeIP:    nodeIP,
		IdentityPath:   identity.Path(),
		LogStore:       logStore,
	})

	executor := command.NewExecutor(conn, registry).
		WithSnapshotMaintenance(snapshot.Path(), version.Version)
	executor.CacheDir = pathOverride("MESHCDN_CACHE_DIR", command.DefaultCacheDir)
	if !skipNginx {
		executor.WithNginxIntegration(store, nodeIP, nginxDir)
	}

	batchText := joinArgs(args)

	execCtx := command.ContextWithLangFromDB(context.Background(), conn)
	result, err := executor.ExecuteBatch(execCtx, batchText)
	if err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	fmt.Print(command.FormatReport(result))

	if result.AnyFailed() {
		os.Exit(1)
	}
	return nil
}

func loadNodeIPBestEffort() string {
	id, err := identity.Load()
	if err != nil {
		return ""
	}
	return id.NodeIP
}

func joinArgs(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	out := args[0]
	for _, a := range args[1:] {
		out += " " + a
	}
	return out
}
