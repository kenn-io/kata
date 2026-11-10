package pgstore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// Exercise the production forward path from the actual prior physical shape.
// The only new object is a nullable TEXT column; legacy rows remain unselected.
func TestTrackerConnectionMigrationFromSchema50(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	source, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "tracker_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := source.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	_, err = source.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300})
	require.NoError(t, err)
	require.NoError(t, source.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `ALTER TABLE tracker_upgrade.issue_sync_bindings DROP COLUMN connection_uid`)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `UPDATE tracker_upgrade.meta SET value='50' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "tracker_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	binding, err := upgraded.IssueSyncBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Nil(t, binding.ConnectionUID)
	var kind, nullable string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT data_type,is_nullable FROM information_schema.columns WHERE table_schema='tracker_upgrade' AND table_name='issue_sync_bindings' AND column_name='connection_uid'`).Scan(&kind, &nullable))
	require.Equal(t, "text", kind)
	require.Equal(t, "YES", nullable)
}
