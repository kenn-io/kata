package jsonl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// ErrCutoverInProgress means a previous JSONL cutover left temp files behind.
// Operators should inspect/remove those files before retrying so we do not
// overwrite evidence from an interrupted swap.
var ErrCutoverInProgress = errors.New("jsonl cutover in progress")

// AutoCutover upgrades an older SQLite database by exporting it to JSONL,
// importing into a fresh current-schema temp DB, then swapping the temp DB into
// place. Databases already at the current schema are left untouched.
func AutoCutover(ctx context.Context, path string) error {
	tmpJSONL := path + ".import.tmp.jsonl"
	tmpDB := path + ".import.tmp.db"
	if err := rejectCutoverTemps(tmpJSONL, tmpDB); err != nil {
		return err
	}
	version, err := sqlitestore.PeekSchemaVersion(ctx, path)
	if err != nil {
		return err
	}
	if version >= db.CurrentSchemaVersion() {
		return nil
	}

	report, err := PreflightSourceFKs(ctx, path)
	if err != nil {
		return err
	}
	if len(report.UnknownViolations) > 0 {
		return formatUnknownViolations(path, report.UnknownViolations)
	}

	cleanupTemps := true
	defer func() {
		if cleanupTemps {
			removeSQLiteFileSet(tmpJSONL)
			removeSQLiteFileSet(tmpDB)
		}
	}()
	if err := exportCutoverSource(ctx, path, tmpJSONL); err != nil {
		return err
	}
	if err := importCutoverTarget(ctx, tmpJSONL, tmpDB); err != nil {
		return err
	}

	backup := fmt.Sprintf("%s.bak.%s", path, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(path, backup); err != nil {
		return fmt.Errorf("backup source db: %w", err)
	}
	if err := os.Rename(tmpDB, path); err != nil {
		_ = os.Rename(backup, path)
		return fmt.Errorf("install cutover db: %w", err)
	}
	cleanupTemps = false
	removeSQLiteFileSet(tmpJSONL)
	if line := formatOrphanSummary(report); line != "" {
		fmt.Fprintln(os.Stderr, line)
	}
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

func exportCutoverSource(ctx context.Context, sourcePath, tmpJSONL string) error {
	source, err := sqlitestore.Open(ctx, sourcePath, db.ReadOnly())
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	f, err := os.OpenFile(tmpJSONL, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // tmpJSONL is daemon-controlled state-dir filename
	if err != nil {
		return fmt.Errorf("create cutover jsonl: %w", err)
	}
	if err := exportForCutover(ctx, source, f, ExportOptions{IncludeDeleted: true}); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync cutover jsonl: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close cutover jsonl: %w", err)
	}
	return nil
}

func importCutoverTarget(ctx context.Context, tmpJSONL, tmpDB string) error {
	target, err := sqlitestore.Open(ctx, tmpDB)
	if err != nil {
		return err
	}
	defer func() { _ = target.Close() }()
	// sqlitestore.Open no longer bootstraps; Migrate is now the single owner
	// of the baseline schema, so the cutover target needs an explicit
	// Migrate before any import row can land.
	if _, err := target.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate cutover target: %w", err)
	}
	in, err := os.Open(tmpJSONL) //nolint:gosec // temp path is generated from trusted DB path
	if err != nil {
		return fmt.Errorf("open cutover jsonl: %w", err)
	}
	defer func() { _ = in.Close() }()
	if err := Import(ctx, in, target); err != nil {
		return err
	}
	// ImportReplay stamps meta.schema_version inside the import transaction, so
	// no follow-up write is needed here.
	return nil
}

// formatOrphanSummary renders the post-cutover summary line.
// Returns "" when no orphans were dropped, so callers can skip
// the println entirely on clean DBs. Only nonzero classes are
// listed, in the fixed order events / comments / links /
// issue_labels. ScrubbedRowsByTable is intentionally not
// included — scrubs preserve the row, so reporting them as
// "discarded" would mislead.
func formatOrphanSummary(report OrphanReport) string {
	// classes is sourced from preflight.go's knownOrphanClasses so the
	// cutover summary stays in sync with the preflight classifier.
	classes := knownOrphanClasses
	var parts []string
	total := 0
	for _, c := range classes {
		n := len(report.DroppedRowsByTable[c])
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
