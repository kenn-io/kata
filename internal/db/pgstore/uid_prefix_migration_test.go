package pgstore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestIssueUIDPrefixIndexMigrationUpgradesVersion44(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "uid_prefix_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, UID: "01hznq7v000000000000000001", Title: "Prefix match", Author: "agent"})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, UID: "01HZNQ7V000000000000000003", Title: "Uppercase match", Author: "agent"})
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, UID: "01HZNQ7V000000000000000000", Title: "Other match", Author: "agent"})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropLaterReadyAndEventIndexes(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `DROP INDEX uid_prefix_upgrade.idx_issues_uid_prefix;
DROP INDEX uid_prefix_upgrade.idx_issues_project_uid_prefix;
UPDATE uid_prefix_upgrade.meta SET value='44' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "uid_prefix_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, fresh.Close())
	for _, targetSchema := range []string{schema, "uid_prefix_fresh"} {
		var definition string
		require.NoError(t, admin.QueryRowContext(ctx, `SELECT replace(indexdef, schemaname || '.', '') FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_issues_uid_prefix'`, targetSchema).Scan(&definition))
		require.Equal(t, `CREATE INDEX idx_issues_uid_prefix ON issues USING btree (uid text_pattern_ops)`, definition)
		require.NoError(t, admin.QueryRowContext(ctx, `SELECT replace(indexdef, schemaname || '.', '') FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_issues_project_uid_prefix'`, targetSchema).Scan(&definition))
		require.Equal(t, `CREATE INDEX idx_issues_project_uid_prefix ON issues USING btree (project_id, uid text_pattern_ops)`, definition)
	}
	issues, err := upgraded.ListAllIssues(ctx, db.ListAllIssuesParams{})
	require.NoError(t, err)
	preserved := make(map[string]string, len(issues))
	for _, issue := range issues {
		preserved[issue.UID] = issue.ProjectUID
	}
	require.Equal(t, map[string]string{
		"01hznq7v000000000000000001": project.UID,
		"01HZNQ7V000000000000000003": project.UID,
		"01HZNQ7V000000000000000000": other.UID,
	}, preserved)
}
