package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// restoreSchema50EventIndex returns a fresh schema's events indexes to their
// schema 50 shape: it drops idx_events_issue_uid_id and recreates
// idx_events_resolved_issue_uid from the released schema 43 migration. Older
// fixtures reach it through dropMCPEventSubscriptionsSchema; it does nothing
// once the schema 51 index is gone, so callers may repeat it.
func restoreSchema50EventIndex(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	var present bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_events_issue_uid_id')`, schema).Scan(&present))
	if !present {
		return
	}
	var resolvedIndexSQL string
	for _, migration := range pgstore.Migrations() {
		if migration.Name == "000043_event_resolved_issue_index.up.sql" {
			resolvedIndexSQL = migration.SQL
		}
	}
	require.NotEmpty(t, resolvedIndexSQL)
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL search_path TO %s; DROP INDEX idx_events_issue_uid_id`, schema)) // #nosec G201 -- fixed test schema.
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, resolvedIndexSQL)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

// eventIssueIndexes returns the definitions of the events issue-identity
// indexes in schema, keyed by name.
func eventIssueIndexes(ctx context.Context, t *testing.T, admin *sql.DB, schema string) map[string]string {
	t.Helper()
	rows, err := admin.QueryContext(ctx, `SELECT indexname, replace(indexdef,$1||'.','') FROM pg_indexes
 WHERE schemaname=$1 AND tablename='events' AND indexname IN ('idx_events_issue_uid','idx_events_issue_uid_id','idx_events_resolved_issue_uid')`, schema)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	indexes := map[string]string{}
	for rows.Next() {
		var name, definition string
		require.NoError(t, rows.Scan(&name, &definition))
		indexes[name] = definition
	}
	require.NoError(t, rows.Err())
	return indexes
}

// Contract: schema 50 upgrades to 51 by replacing the payload-derived issue
// index with one on the header issue_uid and event ID, matching a fresh
// install, and per-issue history still reads through it.
func TestEventIssueUIDIndexMigrationUpgradesVersion50(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "issue_uid_index_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Retained history", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	restoreSchema50EventIndex(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE issue_uid_index_upgrade.meta SET value='50' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.Contains(t, eventIssueIndexes(ctx, t, admin, schema), "idx_events_resolved_issue_uid")

	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, 51, version)
	assert.Equal(t, db.CurrentSchemaVersion(), version)
	events, err := upgraded.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, created.UID, events[0].UID)

	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "issue_uid_index_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	migrated := eventIssueIndexes(ctx, t, admin, schema)
	assert.Equal(t, eventIssueIndexes(ctx, t, admin, "issue_uid_index_fresh"), migrated, "migrated and fresh schemas must agree")
	assert.NotContains(t, migrated, "idx_events_resolved_issue_uid")
	assert.Equal(t, "CREATE INDEX idx_events_issue_uid_id ON events USING btree (issue_uid, id) WHERE (issue_uid IS NOT NULL)", migrated["idx_events_issue_uid_id"])
	assert.Contains(t, migrated, "idx_events_issue_uid")
}
