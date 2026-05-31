// Package daemon contains the in-process kata daemon: namespace resolution,
// runtime file lifecycle, listening endpoint, and HTTP server wiring.
package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kata/internal/config"
)

// Namespace bundles per-dbhash directories used by daemon runtime files and
// (on Unix) the listening socket.
type Namespace struct {
	// DBHash is the 12-char hash of the resolved DB path.
	DBHash string
	// DataDir is <KataHome>/runtime/<dbhash>.
	DataDir string
	// SocketDir is <XDG_RUNTIME_DIR>/kata/<dbhash> or a TMPDIR fallback.
	SocketDir string
}

// NewNamespace resolves directories from $KATA_HOME / $KATA_DB / $XDG_RUNTIME_DIR / $TMPDIR.
// Directories are not created — call EnsureDirs at startup.
func NewNamespace() (*Namespace, error) {
	dbPath, err := config.KataDB()
	if err != nil {
		return nil, fmt.Errorf("resolve KATA_DB: %w", err)
	}
	dataRoot, err := config.RuntimeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve runtime dir: %w", err)
	}
	hash := config.DBHash(dbPath)

	socketDir := socketParent(hash)

	return &Namespace{
		DBHash:    hash,
		DataDir:   dataRoot,
		SocketDir: socketDir,
	}, nil
}

// socketParent returns the per-dbhash socket directory, preferring
// $XDG_RUNTIME_DIR and falling back to $TMPDIR (or os.TempDir) under
// kata-<userTag>/<dbhash>. userTag is the numeric uid on Unix and a
// sanitized username on Windows (where Getuid is meaningless).
func socketParent(dbhash string) string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Join(v, "kata", dbhash)
	}
	tmp := os.Getenv("TMPDIR")
	if tmp == "" {
		tmp = os.TempDir()
	}
	return filepath.Join(tmp, "kata-"+userTag(), dbhash)
}

// EnsureDirs materializes DataDir (0700) and SocketDir (0700).
func (n *Namespace) EnsureDirs() error {
	if err := os.MkdirAll(n.DataDir, 0o700); err != nil {
		return fmt.Errorf("mkdir data dir: %w", err)
	}
	if err := os.MkdirAll(n.SocketDir, 0o700); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	return nil
}
