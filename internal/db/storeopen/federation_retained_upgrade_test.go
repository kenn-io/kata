package storeopen_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/db/storeopen"
)

func TestOpenUpgradesRetainedFederationEventsFrom54(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "kata.db")
	store, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `DROP TABLE federation_retained_events`)
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `UPDATE meta SET value='54' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	upgraded, err := storeopen.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	var exists int
	require.NoError(t, upgraded.(*sqlitestore.Store).QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='federation_retained_events'`).Scan(&exists))
	require.Equal(t, 1, exists)
}
