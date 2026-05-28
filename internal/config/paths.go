// Package config resolves the kata data directory, database path, and
// per-database runtime namespace from environment variables.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// KataHome returns the resolved data directory honoring $KATA_HOME, falling back
// to $HOME/.kata. The directory is not created here; callers materialize it.
func KataHome() (string, error) {
	if v := os.Getenv("KATA_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".kata"), nil
}

// KataDB returns the effective DB path honoring $KATA_DB, falling back to
// <KataHome>/kata.db. Returned path is not validated for existence.
func KataDB() (string, error) {
	if v := os.Getenv("KATA_DB"); v != "" {
		return v, nil
	}
	home, err := KataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "kata.db"), nil
}

// DBHash returns the first 12 lower-hex chars of sha256(absolute(dbPath)).
// Used to namespace runtime files, sockets, and hook output per database.
func DBHash(dbPath string) string {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		abs = dbPath
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:12]
}

// RuntimeDir returns <KataHome>/runtime/<dbhash>. The directory is not created.
func RuntimeDir() (string, error) {
	home, err := KataHome()
	if err != nil {
		return "", err
	}
	db, err := KataDB()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "runtime", DBHash(db)), nil
}

// HookConfigPath returns <KataHome>/hooks.toml. The file is not created here.
func HookConfigPath() (string, error) {
	home, err := KataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "hooks.toml"), nil
}

// DaemonConfigPath returns <KataHome>/config.toml — the optional
// daemon-side settings file. Today only `listen = "..."` is honored
// (used by `kata daemon start` when --listen is not passed). The file
// is not created here.
func DaemonConfigPath() (string, error) {
	home, err := KataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.toml"), nil
}

// FederationCredentialsPath returns <KataHome>/credentials.toml. It stores
// local secrets such as federation bearer tokens and must not be committed to a
// workspace.
func FederationCredentialsPath() (string, error) {
	home, err := KataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "credentials.toml"), nil
}

// HookRootDir returns <KataHome>/hooks/<dbhash>. Per-DB so multiple kata
// databases on the same host don't share output streams. Rejects any
// dbhash that is not a 12-char lower-hex string so a malformed value
// can't escape the hook root via "..", separators, or padding.
func HookRootDir(dbhash string) (string, error) {
	if err := validateDBHash(dbhash); err != nil {
		return "", err
	}
	home, err := KataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "hooks", dbhash), nil
}

// validateDBHash enforces the shape produced by DBHash: exactly 12 chars
// of lower-hex. Rejects any input outside that contract before joining
// it into a path.
func validateDBHash(dbhash string) error {
	if len(dbhash) != 12 {
		return fmt.Errorf("dbhash %q: must be 12 chars (got %d)", dbhash, len(dbhash))
	}
	for _, r := range dbhash {
		isDigit := r >= '0' && r <= '9'
		isHexLetter := r >= 'a' && r <= 'f'
		if !isDigit && !isHexLetter {
			return fmt.Errorf("dbhash %q: must be lower-hex", dbhash)
		}
	}
	return nil
}

// HookOutputDir returns <KataHome>/hooks/<dbhash>/output. Holds per-run
// .out and .err files keyed by <event_id>.<hook_index>.
func HookOutputDir(dbhash string) (string, error) {
	root, err := HookRootDir(dbhash)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "output"), nil
}

// HookRunsPath returns <KataHome>/hooks/<dbhash>/runs.jsonl, the active
// (non-rotated) JSONL log of finished runs. Rotated copies live alongside
// as runs.jsonl.1, runs.jsonl.2, ...
func HookRunsPath(dbhash string) (string, error) {
	root, err := HookRootDir(dbhash)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "runs.jsonl"), nil
}
