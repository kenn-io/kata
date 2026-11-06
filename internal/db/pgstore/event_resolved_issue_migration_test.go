package pgstore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestEventResolvedIssueIndexMigrationUpgradesVersion42(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "history_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Retained history", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropLaterReadyAndEventIndexes(ctx, t, admin, "history_upgrade")
	_, err = admin.ExecContext(ctx, `DROP INDEX IF EXISTS history_upgrade.idx_events_resolved_issue_uid; DROP INDEX IF EXISTS history_upgrade.idx_events_reset_from; DROP INDEX IF EXISTS history_upgrade.idx_issues_uid_prefix; DROP INDEX IF EXISTS history_upgrade.idx_issues_project_uid_prefix; UPDATE history_upgrade.meta SET value='42' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "history_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	events, err := upgraded.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, created.UID, events[0].UID)
	var definition string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname='history_upgrade' AND indexname='idx_events_resolved_issue_uid'`).Scan(&definition))
}
