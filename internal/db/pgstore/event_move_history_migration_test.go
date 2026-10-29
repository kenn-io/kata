package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// dropEventMoveHistoryIndex returns a schema to its pre-35 index set. Every
// older-version fixture needs it because fresh schemas include the index.
func dropEventMoveHistoryIndex(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s.idx_events_moved_from`, schema)) // #nosec G201 -- fixed test schema.
	require.NoError(t, err)
}

// Contract: schema 34 upgrades to 35 by adding the move history index, matching
// a fresh install and preserving resolution of a former short ID.
func TestEventMoveHistoryIndexMigrationUpgradesVersion34(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "move_history_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Task", Author: "worker"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropEventMoveHistoryIndex(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE move_history_upgrade.meta SET value='34' WHERE key='schema_version'`)
	require.NoError(t, err)
	var owner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&owner))
	_, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaOwner: owner, SchemaMode: pgstore.SchemaModeValidate})
	require.Error(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	resolved, err := upgraded.IssueByShortID(ctx, source.ID, issue.ShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, issue.UID, resolved.UID)
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "move_history_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, fresh.Close())
	for _, targetSchema := range []string{schema, "move_history_fresh"} {
		var definition string
		require.NoError(t, admin.QueryRowContext(ctx, `SELECT replace(indexdef, schemaname || '.', '') FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_events_moved_from'`, targetSchema).Scan(&definition))
		require.Equal(t, `CREATE INDEX idx_events_moved_from ON events USING btree ((((payload)::jsonb ->> 'from_project_uid'::text)), (((payload)::jsonb ->> 'from_short_id'::text))) WHERE (type = 'issue.moved'::text)`, definition)
	}
}
