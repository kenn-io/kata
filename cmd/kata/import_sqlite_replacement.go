package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

// installPreparedSQLiteImport retains the live inode for initialized targets.
// A read-only inspection never upgrades the database merely to decide whether
// replacement is safe. The subsequent replay guards ownership transactionally.
func installPreparedSQLiteImport(ctx context.Context, preparedPath, target string, force bool) error {
	if !force {
		return installImportedTarget(preparedPath, target)
	}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		// A missing main file is not permission to overwrite a target that appears
		// later. No-replace publication also refuses orphan SQLite sidecars.
		return installImportedTarget(preparedPath, target)
	} else if err != nil {
		return fmt.Errorf("stat replacement target: %w", err)
	}
	version, err := sqlitestore.PeekSchemaVersion(ctx, target)
	if err != nil {
		return fmt.Errorf("inspect replacement target: %w", err)
	}
	if version != db.CurrentSchemaVersion() {
		return fmt.Errorf("replacement requires current schema %d; target has %d; upgrade the target separately before restoring", db.CurrentSchemaVersion(), version)
	}
	replacement, err := sqlitestore.OpenReplacementTarget(ctx, target)
	if err != nil {
		return err
	}
	defer func() { _ = replacement.Close() }()
	prepared, err := sqlitestore.Open(ctx, preparedPath, db.ReadOnly())
	if err != nil {
		return err
	}
	var snapshot bytes.Buffer
	err = jsonl.Export(ctx, prepared, &snapshot, jsonl.ExportOptions{IncludeDeleted: true})
	closeErr := prepared.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	// The temporary import already chose the instance identity, paused restored
	// integrations and retained attributed history. Preserve that prepared state:
	// applying ordinary/new-instance restore again would change it twice.
	if err := jsonl.ImportWithOptions(ctx, &snapshot, replacement, jsonl.ImportOptions{
		PreserveIssueSyncBindingEnabled:     true,
		PreserveExternalRootBindingsEnabled: true,
	}); err != nil {
		return err
	}
	return removeSQLiteFileSetMain(preparedPath)
}
