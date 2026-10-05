package pgstore_test

import (
	"database/sql"
	_ "embed"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

//go:embed testdata/schema_v32.sql
var relaySchema32 string

func TestRelayMigrationUpgradesFrozenVersion32(t *testing.T) {
	require.Greater(t, db.CurrentSchemaVersion(), 32, "the next real migration must preserve signed state")
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	seed, pin, receipt := dbtest.FrozenRelay32Fixture(t)
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "CREATE SCHEMA relay_upgrade;SET LOCAL search_path TO relay_upgrade,pg_catalog,pg_temp;"+relaySchema32+seed)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "relay_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	dbtest.AssertFrozenRelay32(t, upgraded, pin, receipt)
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "relay_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	require.Equal(t, projectAccessStructure(t, fresh, "relay_fresh"), projectAccessStructure(t, upgraded, "relay_upgrade"))
}
