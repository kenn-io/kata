package pgstore_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// Contract: a schema upgrade finishes even when one migration statement runs
// longer than the per-statement limit on ordinary queries. Migrations commit
// as one transaction, so a canceled statement would roll back every earlier
// migration and the next start would fail the same way. The held lock on
// issues stands in for an index build on a large table.
func TestSchemaUpgradeOutlastsOrdinaryStatementLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "slow_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropLaterReadyAndEventIndexes(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `DROP INDEX slow_upgrade.idx_issues_uid_prefix;
DROP INDEX slow_upgrade.idx_issues_project_uid_prefix;
UPDATE slow_upgrade.meta SET value='44' WHERE key='schema_version'`)
	require.NoError(t, err)

	pgstore.SetStatementTimeoutForTest(t, "1s")
	blocker, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = blocker.ExecContext(ctx, `LOCK TABLE slow_upgrade.issues IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	release := time.AfterFunc(3*time.Second, func() { _ = blocker.Rollback() })
	t.Cleanup(func() {
		release.Stop()
		_ = blocker.Rollback()
	})

	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(context.WithoutCancel(ctx))
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
}
