package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// TestDaemonStartUpgradesLegacyDBThroughStoreopen confirms the daemon's
// startup path runs JSONL cutover when handed a pre-baseline database — the
// cutover step now lives inside storeopen.Open(ApplyMigrations()), so the
// daemon no longer needs to peek+AutoCutover before opening. We assert by
// staging a legacy DB whose cutover will fail (orphan rows preflight halt) and
// verifying the daemon's startup error reaches us; the cutover gate runs
// before any sqlitestore.Open against the path.
func TestDaemonStartUpgradesLegacyDBThroughStoreopen(t *testing.T) {
	dbPath := filepath.Join(setupKataEnv(t), "kata.db")
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	if _, err := d.Migrate(ctx); err != nil {
		_ = d.Close()
		t.Fatalf("migrate fixture: %v", err)
	}
	_, err = d.ExecContext(ctx,
		`UPDATE meta SET value='1' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = runDaemon(ctx)

	require.Error(t, err)
	// The cutover step runs inside storeopen.Open(ApplyMigrations()) before
	// the backend handle is returned, so when AutoCutover fails the error
	// reaches the daemon caller. We assert on the export-side prefix because
	// that is the first thing AutoCutover does after detecting an old
	// schema_version — any failure before the migrate runner sees the DB is
	// enough to prove the cutover gate ran first.
	assert.Contains(t, err.Error(), "export projects")
	ver, peekErr := sqlitestore.PeekSchemaVersion(context.Background(), dbPath)
	require.NoError(t, peekErr)
	assert.Equal(t, 1, ver)
}
