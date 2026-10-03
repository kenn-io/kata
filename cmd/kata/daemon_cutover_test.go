package main

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitelock"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/version"
)

func TestDaemonMigrationConsentPreservesData(t *testing.T) {
	for _, build := range []string{"dev", "v0.18.0"} {
		t.Run(build, func(t *testing.T) {
			home := setupKataEnv(t)
			t.Setenv("KATA_ALLOW_DEV_MIGRATION", "")
			original := version.Version
			version.Version = build
			t.Cleanup(func() { version.Version = original })
			path := filepath.Join(home, "kata.db")
			s, err := sqlitestore.Open(t.Context(), path)
			require.NoError(t, err)
			_, err = s.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			_, err = s.ExecContext(t.Context(), `UPDATE meta SET value=? WHERE key='schema_version'`, db.CurrentSchemaVersion()-1)
			require.NoError(t, err)
			require.NoError(t, s.Close())
			// Force startup to return after opening storage so the test can
			// inspect the completed upgrade without polling a running daemon.
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer func() { _ = occupied.Close() }()
			args := []string{"daemon", "start", "--foreground", "--listen", occupied.Addr().String()}
			if build == "dev" {
				args = append(args, "--allow-dev-migration")
			}
			_, _, err = executeRootCapture(t, t.Context(), args...)
			require.ErrorContains(t, err, "listen")
			s, err = sqlitestore.Open(t.Context(), path, db.ReadOnly())
			require.NoError(t, err)
			defer func() { _ = s.Close() }()
			ver, err := s.SchemaVersion(t.Context())
			require.NoError(t, err)
			require.Equal(t, db.CurrentSchemaVersion(), ver)
			project, err := s.ProjectByName(t.Context(), "example-project")
			require.NoError(t, err)
			require.Equal(t, "example-project", project.Name)
			lock, err := sqlitelock.Acquire(path)
			require.NoError(t, err)
			lock.Release()
		})
	}
}

// TestDaemonStartUpgradesLegacyDBThroughStoreopen confirms the daemon's
// startup path runs JSONL cutover when handed a pre-current database — the
// cutover step lives inside storeopen.Open, so the daemon no longer needs
// to peek+AutoCutover before opening. We assert by staging a legacy DB
// whose cutover will fail (orphan rows preflight halt) and verifying the
// daemon's startup error reaches us; the cutover gate runs before any
// sqlitestore.Open against the path.
func TestDaemonStartUpgradesLegacyDBThroughStoreopen(t *testing.T) {
	t.Setenv("KATA_ALLOW_DEV_MIGRATION", "1")
	dbPath := filepath.Join(setupKataEnv(t), "kata.db")
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx,
		`UPDATE meta SET value='1' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = runDaemon(ctx)

	require.Error(t, err)
	// The cutover step runs inside storeopen.Open before the backend
	// handle is returned, so when AutoCutover fails the error reaches the
	// daemon caller. We assert on the export-side prefix because that is
	// the first thing AutoCutover does after detecting an old
	// schema_version — any failure before sqlitestore.Open sees the DB
	// is enough to prove the cutover gate ran first.
	assert.Contains(t, err.Error(), "export projects")
	ver, peekErr := sqlitestore.PeekSchemaVersion(context.Background(), dbPath)
	require.NoError(t, peekErr)
	assert.Equal(t, 1, ver)
}
