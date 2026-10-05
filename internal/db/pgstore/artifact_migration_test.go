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

//go:embed testdata/schema_v33.sql
var artifactSchema33 string

func TestArtifactMigrationUpgradesFrozenVersion33(t *testing.T) {
	require.Greater(t, db.CurrentSchemaVersion(), 33, "real artifact migration must upgrade released relay state")
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	seed, pin, receipt := dbtest.FrozenRelay32Fixture(t)
	_, err = tx.ExecContext(ctx, "CREATE SCHEMA artifact_upgrade;SET LOCAL search_path TO artifact_upgrade,pg_catalog,pg_temp;"+artifactSchema33+seed+"UPDATE meta SET value='33' WHERE key='schema_version';")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "artifact_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	dbtest.AssertFrozenRelay32(t, upgraded, pin, receipt)
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	dbtest.RunEmbeddingArtifactStorage(t, upgraded)
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "artifact_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	require.Equal(t, projectAccessStructure(t, fresh, "artifact_fresh"), projectAccessStructure(t, upgraded, "artifact_upgrade"))
}
