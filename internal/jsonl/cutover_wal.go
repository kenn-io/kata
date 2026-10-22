package jsonl

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

// Compare checkpointed main-file contents, rather than just WAL presence: a
// writer can commit and close during import, checkpointing/removing its WAL.
func cutoverSourceFingerprint(ctx context.Context, path string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	file, err := os.Open(path) //nolint:gosec // path is the trusted source database.
	if err != nil {
		return result, fmt.Errorf("fingerprint cutover source: %w", err)
	}
	defer func() { _ = file.Close() }()
	before, err := file.Stat()
	if err != nil {
		return result, fmt.Errorf("stat cutover source: %w", err)
	}
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		n, err := file.Read(buffer)
		_, _ = hash.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("fingerprint cutover source: %w", err)
		}
	}
	after, err := file.Stat()
	if err != nil {
		return result, fmt.Errorf("stat cutover source: %w", err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return result, errSourceChangedDuringCutover
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}

// checkpointCutoverSource opens the legacy file without schema bootstrap. A
// nonempty WAL belongs to the old page layout and must never be replayed on
// the replacement, even when the export has already read its committed rows.
func checkpointCutoverSource(ctx context.Context, path string) error {
	escaped := (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()
	source, err := sql.Open("sqlite", "file:"+escaped+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("checkpoint source WAL: %w", err)
	}
	source.SetMaxOpenConns(1)
	var busy, frames, checkpointed int
	queryErr := source.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed)
	closeErr := source.Close()
	if queryErr != nil {
		return fmt.Errorf("checkpoint source WAL: %w; stop all database users before retrying", queryErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close source WAL checkpoint connection: %w", closeErr)
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint source WAL: busy=%d frames=%d checkpointed=%d; stop all database users before retrying", busy, frames, checkpointed)
	}
	return refuseCutoverSourceSidecars(path)
}

func refuseCutoverSourceSidecars(path string) error {
	// Closing the last connection removes WAL/SHM. Remaining sidecars mean
	// another connection may still own the old file; never remove them by hand
	// or install a different main file alongside them.
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect source WAL sidecar: %w", err)
		}
		return fmt.Errorf("checkpoint source WAL: %s remains; stop all database users before retrying", path+suffix)
	}
	return nil
}
