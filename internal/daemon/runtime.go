package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RuntimeRecord is the on-disk shape of daemon.<pid>.json.
type RuntimeRecord struct {
	PID       int       `json:"pid"`
	Address   string    `json:"address"` // unix:///path or 127.0.0.1:7474
	DBPath    string    `json:"db_path"`
	Version   string    `json:"version,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// WriteRuntimeFile writes <dir>/daemon.<pid>.json atomically. The temp file
// uses os.CreateTemp so concurrent same-PID writers don't race on a shared
// .tmp path; the loser's rename simply replaces the winner's.
func WriteRuntimeFile(dir string, rec RuntimeRecord) (string, error) {
	if rec.PID <= 0 {
		return "", fmt.Errorf("pid must be > 0")
	}
	final := filepath.Join(dir, fmt.Sprintf("daemon.%d.json", rec.PID))
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	tmp, err := os.CreateTemp(dir, fmt.Sprintf("daemon.%d.*.json.tmp", rec.PID))
	if err != nil {
		return "", fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil { //nolint:gosec // runtime files are world-readable per §2.3
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("chmod tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil { //nolint:gosec // tmpPath comes from os.CreateTemp inside dir
		cleanup()
		return "", fmt.Errorf("rename: %w", err)
	}
	return final, nil
}

// ReadRuntimeFile parses one file.
func ReadRuntimeFile(path string) (RuntimeRecord, error) {
	body, err := os.ReadFile(path) //nolint:gosec // path is a runtime file selected by the caller
	if err != nil {
		return RuntimeRecord{}, fmt.Errorf("read %s: %w", path, err)
	}
	var rec RuntimeRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return RuntimeRecord{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return rec, nil
}

// ListRuntimeFiles returns RuntimeRecords for each daemon.*.json in dir.
// Garbage / parse-failed files are skipped silently.
func ListRuntimeFiles(dir string) ([]RuntimeRecord, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read dir: %w", err)
	}
	var out []RuntimeRecord
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "daemon.") || !strings.HasSuffix(name, ".json") {
			continue
		}
		// must parse the pid out of the filename to filter .tmp etc.
		mid := strings.TrimSuffix(strings.TrimPrefix(name, "daemon."), ".json")
		if _, err := strconv.Atoi(mid); err != nil {
			continue
		}
		rec, err := ReadRuntimeFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// ProcessAlive reports whether pid currently refers to a live process.
// Implementation is platform-specific: Unix uses kill(pid, 0); Windows uses
// OpenProcess + GetExitCodeProcess. Both are best-effort and don't try to
// distinguish "not ours" vs "alive".
//
// See runtime_unix.go and runtime_windows.go.

// CleanupStaleFiles removes any daemon.<pid>.json whose pid is dead. It
// cross-checks the filename's pid against the record's pid so a malformed file
// reporting a different pid can never delete an unrelated daemon's file: when
// they disagree we leave the file alone for human inspection.
func CleanupStaleFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "daemon.") || !strings.HasSuffix(name, ".json") {
			continue
		}
		mid := strings.TrimSuffix(strings.TrimPrefix(name, "daemon."), ".json")
		filenamePID, err := strconv.Atoi(mid)
		if err != nil {
			continue
		}
		path := filepath.Join(dir, name)
		rec, err := ReadRuntimeFile(path)
		if err != nil || rec.PID != filenamePID {
			// Leave malformed or PID-mismatched files for human inspection.
			continue
		}
		if !ProcessAlive(filenamePID) {
			_ = os.Remove(path)
		}
	}
	return nil
}
