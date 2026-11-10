package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testfix"
)

func TestFederationLocalReplaysUpgrade(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "kata.db")
	store, err := testfix.OpenSQLite(ctx, path)
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `DROP TABLE federation_local_replays`)
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `UPDATE meta SET value='50' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	require.NoError(t, sqlitestore.UpgradeInPlace(ctx, path))
	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	_, err = upgraded.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	var count int
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_local_replays`).Scan(&count))
	require.Zero(t, count)
}
