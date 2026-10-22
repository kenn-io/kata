package jsonl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kit/atomicfile"
)

// ErrCutoverInProgress identifies an active cutover or unresolved swap state.
// Interrupted imports are preserved before retrying from an intact old source.
var ErrCutoverInProgress = errors.New("jsonl cutover in progress")

var errSourceChangedDuringCutover = errors.New("source changed during cutover; stop all database users and retry; original source retained")

// AutoCutover upgrades an older SQLite database by exporting it to JSONL,
// importing into a fresh current-schema temp DB, then swapping the temp DB into
// place. Databases already at the current schema are left untouched.
func AutoCutover(ctx context.Context, path string) error {
	return autoCutover(ctx, path, cutoverOperations{exportCutoverSource, atomicfile.Replace})
}

type cutoverOperations struct {
	exportSource func(context.Context, string, string) (int64, error)
	replaceFile  func(string, string) error
}

func autoCutover(ctx context.Context, path string, ops cutoverOperations) error {
	return WithCutoverLock(ctx, path, func() error {
		return autoCutoverLocked(ctx, path, ops)
	})
}

// WithCutoverLock runs fn while holding the SQLite source's cutover lock.
// It coordinates source inspections with AutoCutover. fn must not call
// AutoCutover or WithCutoverLock for the same path.
func WithCutoverLock(ctx context.Context, path string, fn func() error) error {
	// Keep the lock file: unlinking it after unlock lets two processes lock
	// different inodes at the same pathname.
	lock := flock.New(path + ".import.lock")
	defer func() { _ = lock.Close() }()
	// Readiness polling briefly probes this same lock. Retry transient
	// contention before treating it as another importer holding the source.
	lockCtx, cancel := context.WithTimeout(ctx, time.Second)
	locked, err := lock.TryLockContext(lockCtx, 10*time.Millisecond)
	cancel()
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("lock SQLite cutover: %w", err)
	}
	if !locked {
		return fmt.Errorf("%w: another process holds %s; wait for migration and inspect kata daemon logs", ErrCutoverInProgress, lock.Path())
	}
	return fn()
}

func autoCutoverLocked(ctx context.Context, path string, ops cutoverOperations) error {
	tmpJSONL := path + ".import.tmp.jsonl"
	tmpDB := path + ".import.tmp.db"
	if err := CheckCutoverSource(ctx, path); err != nil {
		return err
	}
	version, err := sqlitestore.PeekSchemaVersion(ctx, path)
	if err != nil {
		return err
	}
	if version >= db.CurrentSchemaVersion() {
		return rejectCutoverTemps(cutoverTempPaths(path)...)
	}

	if err := checkpointCutoverSource(ctx, path); err != nil {
		return err
	}
	// The swap renames only the main file. Fold the WAL in first so the
	// backup is complete and no old WAL pages are left beside the new DB.
	if err := sqlitestore.CheckpointForCutover(ctx, path); err != nil {
		return err
	}

	report, err := PreflightSourceFKs(ctx, path)
	if err != nil {
		return err
	}
	if len(report.UnknownViolations) > 0 {
		return formatUnknownViolations(path, report.UnknownViolations)
	}

	if err := preserveCutoverTemps(path); err != nil {
		return err
	}
	sourceFingerprint, err := cutoverSourceFingerprint(ctx, path)
	if err != nil {
		return err
	}

	cleanupTemps := true
	defer func() {
		if cleanupTemps {
			removeSQLiteFileSet(tmpJSONL)
			removeSQLiteFileSet(tmpDB)
		}
	}()
	sourceEvents, err := ops.exportSource(ctx, path, tmpJSONL)
	if err != nil {
		return err
	}
	// Preflight identifies the only event drops allowed during cutover.
	// Check against the source DB, not the potentially incomplete export.
	expectedEvents := sourceEvents - int64(report.DropCount("events"))
	if err := importCutoverTarget(ctx, tmpJSONL, tmpDB, expectedEvents); err != nil {
		return err
	}

	// Recheck after export/import: a concurrent source user may have opened
	// or written the WAL since the first checkpoint.
	if err := checkpointCutoverSource(ctx, path); err != nil {
		return err
	}
	currentFingerprint, err := cutoverSourceFingerprint(ctx, path)
	if err != nil {
		return err
	}
	if currentFingerprint != sourceFingerprint {
		return errSourceChangedDuringCutover
	}
	// Fingerprinting can take time on a large source. Reject a database user
	// that opened WAL/SHM again while the file was being read.
	if err := refuseCutoverSourceSidecars(path); err != nil {
		return err
	}

	backup := fmt.Sprintf("%s.bak.%s", path, time.Now().UTC().Format("20060102T150405Z"))
	if err := ops.replaceFile(path, backup); err != nil {
		return fmt.Errorf("backup source db: %w", err)
	}
	if err := ops.replaceFile(tmpDB, path); err != nil {
		if restoreErr := ops.replaceFile(backup, path); restoreErr != nil {
			cleanupTemps = false
			return fmt.Errorf("install cutover db: %w; restore source db: %w; backup remains at %s and import files at %s; recover original source before starting", err, restoreErr, backup, tmpDB)
		}
		return fmt.Errorf("install cutover db: %w", err)
	}
	cleanupTemps = false
	removeSQLiteFileSet(tmpJSONL)
	if line := formatOrphanSummary(report); line != "" {
		fmt.Fprintln(os.Stderr, line)
	}
	return nil
}

// CheckCutoverSource prevents a missing source with cutover recovery artifacts
// from being mistaken for a fresh database by the storage opener.
func CheckCutoverSource(ctx context.Context, path string) error {
	artifacts := cutoverTempPaths(path)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect cutover backups: %w", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(path)+".bak.") {
			artifacts = append(artifacts, filepath.Join(filepath.Dir(path), entry.Name()))
		}
	}
	for _, temp := range artifacts {
		if _, err := os.Lstat(temp); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect cutover temp: %w", err)
		}
		version, err := sqlitestore.PeekSchemaVersion(ctx, path)
		if err == nil && version <= 0 {
			err = errors.New("source has no recognizable schema_version")
		}
		if err != nil {
			return fmt.Errorf("%w: source database is unavailable or unrecognized at %s; preserve import temp files and *.bak.* backups and recover the original source before starting: %w", ErrCutoverInProgress, path, err)
		}
		return nil
	}
	return nil
}

func cutoverTempPaths(path string) []string {
	return []string{path + ".import.tmp.jsonl", path + ".import.tmp.db", path + ".import.tmp.db-wal", path + ".import.tmp.db-shm"}
}

func preserveCutoverTemps(path string) error {
	var paths []string
	for _, temp := range cutoverTempPaths(path) {
		info, err := os.Lstat(temp)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect cutover temp: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular import file; preserve and inspect it before retrying", ErrCutoverInProgress, temp)
		}
		paths = append(paths, temp)
	}
	if len(paths) == 0 {
		return nil
	}
	dir, err := os.MkdirTemp(filepath.Dir(path), filepath.Base(path)+".import.recovery-")
	if err != nil {
		return fmt.Errorf("preserve interrupted cutover: %w", err)
	}
	for _, temp := range paths {
		if err := atomicfile.RenameNoReplace(temp, filepath.Join(dir, filepath.Base(temp))); err != nil {
			return fmt.Errorf("preserve interrupted cutover in %s: %w", dir, err)
		}
	}
	fmt.Fprintf(os.Stderr, "kata: preserved interrupted SQLite import in %s; retrying migration from original source\n", dir)
	return nil
}

func rejectCutoverTemps(paths ...string) error {
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%w: %s exists", ErrCutoverInProgress, path)
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("stat cutover temp: %w", err)
		}
	}
	return nil
}

func exportCutoverSource(ctx context.Context, sourcePath, tmpJSONL string) (int64, error) {
	source, err := sqlitestore.Open(ctx, sourcePath, db.ReadOnly())
	if err != nil {
		return 0, err
	}
	defer func() { _ = source.Close() }()
	var sourceEvents int64
	if err := source.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&sourceEvents); err != nil {
		return 0, fmt.Errorf("count source events: %w", err)
	}
	f, err := os.OpenFile(tmpJSONL, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // tmpJSONL is daemon-controlled state-dir filename
	if err != nil {
		return 0, fmt.Errorf("create cutover jsonl: %w", err)
	}
	if err := exportForCutover(ctx, source, f, ExportOptions{IncludeDeleted: true}); err != nil {
		_ = f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("sync cutover jsonl: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("close cutover jsonl: %w", err)
	}
	return sourceEvents, nil
}

func importCutoverTarget(ctx context.Context, tmpJSONL, tmpDB string, expectedEvents int64) error {
	// sqlitestore.Open bootstraps the canonical schema in one transaction
	// when the file is fresh, so a freshly created tmpDB is ready for the
	// import to land directly.
	target, err := sqlitestore.Open(ctx, tmpDB)
	if err != nil {
		return err
	}
	defer func() { _ = target.Close() }()
	in, err := os.Open(tmpJSONL) //nolint:gosec // temp path is generated from trusted DB path
	if err != nil {
		return fmt.Errorf("open cutover jsonl: %w", err)
	}
	defer func() { _ = in.Close() }()
	if err := ImportWithOptions(ctx, in, target, ImportOptions{
		PreserveIssueSyncBindingEnabled:     true,
		PreserveExternalRootBindingsEnabled: true,
	}); err != nil {
		return err
	}
	var importedEvents int64
	if err := target.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&importedEvents); err != nil {
		return fmt.Errorf("count imported events: %w", err)
	}
	if importedEvents != expectedEvents {
		return fmt.Errorf("cutover event count mismatch: expected %d after preflight orphan drops, imported %d; source database unchanged", expectedEvents, importedEvents)
	}
	return nil
}

// formatOrphanSummary renders the post-cutover summary line.
// Returns "" when no orphans were dropped, so callers can skip
// the println entirely on clean DBs. Only nonzero classes are
// listed, in the fixed order events / comments / links /
// issue_labels. Scrubbed rows are intentionally not included — scrubs
// preserve the row, so reporting them as "discarded" would mislead.
func formatOrphanSummary(report OrphanReport) string {
	// classes is sourced from preflight.go's knownOrphanClasses so the
	// cutover summary stays in sync with the preflight classifier.
	classes := knownOrphanClasses
	var parts []string
	total := 0
	for _, c := range classes {
		n := report.DropCount(c)
		if n == 0 {
			continue
		}
		total += n
		parts = append(parts, fmt.Sprintf("%s: %d", c, n))
	}
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("kata cutover: discarded %d orphan rows from old DB (%s)",
		total, strings.Join(parts, ", "))
}

// formatUnknownViolations renders the preflight halt error.
// Caps per-child-table output at 20 rows to bound log size on
// widely-corrupted DBs. Includes a remediation hint pointing at
// the sqlite3 PRAGMA the operator can run by hand.
func formatUnknownViolations(path string, violations []FKViolation) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "preflight: source DB at %s has unhandled foreign-key corruption that cutover cannot resolve. ", path)
	sb.WriteString("Inspect with `sqlite3 ")
	sb.WriteString(path)
	sb.WriteString(" 'PRAGMA foreign_key_check;'` and repair before retrying. Found:")
	truncated := false
	perTable := map[string]int{}
	for _, v := range violations {
		if perTable[v.Table] >= 20 {
			truncated = true
			continue
		}
		perTable[v.Table]++
		col := v.Column
		if col == "" {
			col = "?"
		}
		rowidStr := "NULL"
		if v.RowID.Valid {
			rowidStr = fmt.Sprintf("%d", v.RowID.Int64)
		}
		fmt.Fprintf(&sb, "\n  %s rowid=%s parent=%s column=%s", v.Table, rowidStr, v.ParentTable, col)
	}
	if truncated {
		sb.WriteString("\n  (output capped at 20 rows per table)")
	}
	return errors.New(sb.String())
}

func removeSQLiteFileSet(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
