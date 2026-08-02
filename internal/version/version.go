// Package version exposes build metadata and source self-containment.
//
// CommitHash and BuildTime are populated by the Makefile via -ldflags:
//
//	go build -ldflags "-X github.com/example/meshcdn/internal/version.CommitHash=$(git rev-parse HEAD)"
//
// Source is populated by go:embed at compile time. The Makefile's
// source-snapshot target copies cmd/, internal/, scripts/, docs/ into
// ./source/ before invoking go build, so the embedded tree exactly
// matches the binary being compiled.
package version

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

var (
	Version    = "v4.3.1"
	CommitHash = "unknown"
	BuildTime  = "unknown"
)

// Banner returns the version line shown by --version.
func Banner() string {
	return fmt.Sprintf("MeshCDN %s (commit: %s, built: %s)", Version, CommitHash, BuildTime)
}

// Source contains the full source tree as it existed at build time.
//
// Note: go:embed cannot reference parent directories ("../"), so the
// Makefile copies the snapshot to internal/version/source/ before build.
//
//go:embed all:source
var Source embed.FS

// DumpSource walks the embedded source FS and writes everything out to dest.
// Creates dest and intermediate directories as needed.
//
// Used by `cdn-agent dump-source` per V4-DESIGN §4.4 (any binary can recover
// its corresponding source).
func DumpSource(dest string) error {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("create dest dir %s: %w", dest, err)
	}

	root := "source"
	return fs.WalkDir(Source, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Strip the embedded "source/" prefix so output mirrors the original layout.
		rel := path[len(root):]
		if rel == "" {
			return nil // skip root itself
		}
		if rel[0] == '/' {
			rel = rel[1:]
		}
		outPath := filepath.Join(dest, rel)

		if d.IsDir() {
			return os.MkdirAll(outPath, 0755)
		}

		// Regular file
		if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
			return err
		}
		src, err := Source.Open(path)
		if err != nil {
			return fmt.Errorf("open embedded %s: %w", path, err)
		}
		defer src.Close()

		dst, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return fmt.Errorf("create %s: %w", outPath, err)
		}
		defer dst.Close()

		if _, err := io.Copy(dst, src); err != nil {
			return fmt.Errorf("copy %s: %w", outPath, err)
		}
		return nil
	})
}
