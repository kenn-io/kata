package pgstore_test

import (
	"database/sql"
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestDateZoneMigrationPreservesIssues(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "date_zone_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "zone-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Scheduled", Author: "worker-a", Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-01-15[Europe/Paris]"`)}})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `DROP INDEX date_zone_upgrade.idx_issues_zone_candidates; UPDATE date_zone_upgrade.meta SET value='51' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	got, err := upgraded.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{"scheduled_on":"2026-01-15[Europe/Paris]"}`, string(got.Metadata))
	features, err := db.ProjectRequiredEventFeatures(ctx, upgraded, project.UID)
	require.NoError(t, err)
	require.ErrorIs(t, db.RequireEventFeatures("", features), db.ErrUnsupportedEventFeatures)
	tx, err := upgraded.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SET LOCAL enable_seqscan=off`)
	require.NoError(t, err)
	rows, err := tx.QueryContext(ctx, `EXPLAIN SELECT i.metadata FROM issues i JOIN projects p ON p.id=i.project_id WHERE p.uid=$1 AND ((json(i.metadata) ->> 'scheduled_on') LIKE '%]' OR (json(i.metadata) ->> 'deadline_on') LIKE '%]')`, project.UID)
	require.NoError(t, err)
	var lines []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		lines = append(lines, line)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Contains(t, strings.Join(lines, "\n"), "idx_issues_zone_candidates")
}
