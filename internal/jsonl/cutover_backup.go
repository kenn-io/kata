package jsonl

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func checkCutoverIntegrity(ctx context.Context, path string) error {
	source, err := sqlitestore.Open(ctx, path, db.ReadOnly())
	if err != nil {
		return fmt.Errorf("integrity_check %s: %w", path, err)
	}
	defer func() { _ = source.Close() }()
	rows, err := source.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("integrity_check %s: %w", path, err)
	}
	defer func() { _ = rows.Close() }()
	checked := false
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("integrity_check %s: %w", path, err)
		}
		if result != "ok" {
			return fmt.Errorf("integrity_check %s: %s; source database unchanged, repair before migrating", path, result)
		}
		checked = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("integrity_check %s: %w", path, err)
	}
	if !checked {
		return fmt.Errorf("integrity_check %s returned no result", path)
	}
	return nil
}

// VACUUM INTO copies a consistent SQLite image, including committed WAL data.
// AutoCutover has already checked the source; validate the backup before use.
func backupCutoverSource(ctx context.Context, path, backup string) (returnErr error) {
	source, err := sqlitestore.Open(ctx, path, db.ReadOnly())
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	// Reserve a private file without overwriting an earlier backup. SQLite
	// accepts an empty destination for VACUUM INTO.
	file, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600) //nolint:gosec // G304: backup is derived from the requested database path and created exclusively.
	if err != nil {
		return fmt.Errorf("create migration backup: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			removeSQLiteFileSet(backup)
		}
	}()
	if _, err := source.ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
		return fmt.Errorf("copy migration backup: %w", err)
	}
	if err := checkCutoverIntegrity(ctx, backup); err != nil {
		return err
	}
	file, err = os.OpenFile(backup, os.O_RDWR, 0600) //nolint:gosec // G304: reopen the private backup created above.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync migration backup: %w", err)
	}
	return nil
}

func checkpointCutoverSource(ctx context.Context, path string) error {
	uri := (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()
	source, err := sql.Open("sqlite", "file:"+uri+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	var busy, frames, checkpointed int
	if err := source.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint migration source: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("migration source is busy; stop all database users before migrating")
	}
	if err := source.Close(); err != nil {
		return err
	}
	// No old journal may be replayed into the replacement database.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove source journal: %w", err)
		}
	}
	return nil
}
