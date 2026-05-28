// MeshCDN agent entry point.
//
// Subcommands:
//
//	exec <command>           Execute /w/d/v command (or batch)
//	serve                    Run as daemon (renewal scanner/worker)
//	install-bootstrap [...]  Setup helpers called by install.sh
//	dump-source [path]       Extract embedded source tree
//	--version                Print version info
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/example/meshcdn/internal/cert"
	"github.com/example/meshcdn/internal/cli"
	"github.com/example/meshcdn/internal/db"
	"github.com/example/meshcdn/internal/nginx"
	"github.com/example/meshcdn/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(0)
	}

	switch os.Args[1] {
	case "--version", "version":
		fmt.Println(version.Banner())

	case "dump-source":
		dest := "./meshcdn-source"
		if len(os.Args) > 2 {
			dest = os.Args[2]
		}
		if err := version.DumpSource(dest); err != nil {
			fmt.Fprintf(os.Stderr, "dump-source failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Source extracted to: %s\n", dest)

	case "exec":
		if err := cli.Exec(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "serve":
		if err := cli.Serve(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "install-bootstrap":
		if err := installBootstrap(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "install-bootstrap failed: %v\n", err)
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(version.Banner())
	fmt.Println()
	fmt.Println("Usage: cdn-agent <subcommand> [args...]")
	fmt.Println()
	fmt.Println("Subcommands:")
	fmt.Println("  exec <command>            Execute /w, /d, /v command")
	fmt.Println("  serve                     Run as daemon (cert renewal etc.)")
	fmt.Println("  install-bootstrap [...]   (called by install.sh)")
	fmt.Println("  dump-source [path]        Extract embedded source tree")
	fmt.Println("  --version                 Print version info")
}

func installBootstrap(args []string) error {
	fs := flag.NewFlagSet("install-bootstrap", flag.ExitOnError)
	nodeIP := fs.String("node-ip", "", "public IP of this node")
	certsDir := fs.String("certs-dir", "/etc/meshcdn/persistent/certs", "persistent/certs directory")
	nginxDir := fs.String("nginx-dir", "/etc/meshcdn/runtime/nginx", "runtime/nginx directory")
	dbPath := fs.String("db-path", "/etc/meshcdn/runtime/config.db", "config.db path")
	welcomeDir := fs.String("welcome-dir", "/etc/meshcdn/runtime/welcome", "welcome directory")
	regenNginx := fs.Bool("regen-nginx", false, "regenerate nginx config")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *nodeIP == "" {
		return fmt.Errorf("--node-ip required")
	}

	store, err := cert.NewStore(*certsDir)
	if err != nil {
		return fmt.Errorf("open cert store: %w", err)
	}
	meta, err := cert.EnsureSelfSigned(store, *nodeIP)
	if err != nil {
		return fmt.Errorf("ensure self-signed: %w", err)
	}
	fmt.Printf("Self-signed cert for %s: fingerprint=%s, expires=%s\n",
		*nodeIP, meta.FingerprintPrefix, meta.NotAfter.Format("2006-01-02"))

	if *regenNginx {
		if err := os.MkdirAll(filepath.Dir(*dbPath), 0755); err != nil {
			return fmt.Errorf("mkdir db dir: %w", err)
		}
		conn, err := db.Open(*dbPath)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer conn.Close()

		gen := nginx.New(store, *nodeIP)
		gen.OutputDir = *nginxDir
		gen.WelcomeRoot = *welcomeDir
		// ChallengeRoot defaults to /etc/meshcdn/runtime/challenges in New()

		if err := gen.Generate(context.Background(), conn); err != nil {
			return fmt.Errorf("generate nginx config: %w", err)
		}

		mainConf := filepath.Join(*nginxDir, "nginx.conf")
		if err := nginx.Validate(mainConf); err != nil {
			return fmt.Errorf("validate nginx config: %w", err)
		}
		fmt.Printf("nginx config generated and validated at %s\n", *nginxDir)
	}

	return nil
}
