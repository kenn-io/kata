package pgstore_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
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
	for _, issueUID := range []string{"01HZNQ7V000000000000000002", "01HZNQ7V000000000000000001", "01hznq7v000000000000000003"} {
		_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, UID: issueUID, Title: "Prefix match", Author: "agent"})
		require.NoError(t, err)
	}
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, UID: "01HZNQ7V000000000000000000", Title: "Other match", Author: "agent"})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `DROP INDEX uid_prefix_upgrade.idx_issues_uid_prefix;
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
	}
	matches, err := upgraded.IssueUIDPrefixMatch(ctx, "01HZNQ7V", 1, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Equal(t, "01HZNQ7V000000000000000000", matches[0].UID)
	matches, err = upgraded.IssueUIDPrefixMatch(db.WithAuthorizedProjects(ctx, []string{project.UID}), "01HZNQ7V", 1, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Equal(t, "01HZNQ7V000000000000000001", matches[0].UID)
	matches, err = upgraded.IssueUIDPrefixMatch(ctx, "01hznq7v", 20, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Equal(t, "01hznq7v000000000000000003", matches[0].UID)
	matches, err = upgraded.IssueUIDPrefixMatch(db.WithAuthorizedProjects(ctx, []string{other.UID}), "01hznq7v", 20, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Empty(t, matches)

	_, err = upgraded.ExecContext(ctx, `INSERT INTO issues(uid,short_id,project_id,title,author)
SELECT '01M4'||lpad(x::text,22,'0'),lpad(x::text,6,'0'),$1,'Unrelated issue','agent' FROM generate_series(1,100000) n(x)`, project.ID)
	require.NoError(t, err)
	// Put the match after unrelated UIDs so LIMIT cannot hide a full index scan.
	_, _, err = upgraded.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, UID: "01ZZ0000000000000000000001", Title: "Last match", Author: "agent"})
	require.NoError(t, err)
	_, err = upgraded.ExecContext(ctx, `ANALYZE issues`)
	require.NoError(t, err)
	for _, projects := range [][]string{nil, {project.UID}} {
		lookupContext := db.WithAuthorizedProjects(ctx, projects)
		for range 12 {
			matches, err := upgraded.IssueUIDPrefixMatch(lookupContext, "01ZZ", 1, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Len(t, matches, 1)
			require.Equal(t, "01ZZ0000000000000000000001", matches[0].UID)
			matches, err = upgraded.IssueUIDPrefixMatch(lookupContext, "01ZY", 1, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Empty(t, matches)
		}
	}
	// Inspect the real pgx-cached queries after PostgreSQL can choose generic plans.
	rows, err := upgraded.QueryContext(ctx, `SELECT name, cardinality(parameter_types) FROM pg_prepared_statements
WHERE statement LIKE '%i.uid LIKE %' AND statement NOT LIKE '%pg_prepared_statements%'`)
	require.NoError(t, err)
	statements := make(map[string]int)
	for rows.Next() {
		var name string
		var parameters int
		require.NoError(t, rows.Scan(&name, &parameters))
		statements[name] = parameters
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, statements, 2)
	for name, parameters := range statements {
		for _, prefix := range []string{"01ZZ%", "01ZY%"} {
			arguments := fmt.Sprintf("'%s',1", prefix)
			if parameters == 3 {
				arguments = fmt.Sprintf("'%s','%s',1", prefix, project.UID)
			}
			rows, err := upgraded.QueryContext(ctx, "EXPLAIN EXECUTE "+pgx.Identifier{name}.Sanitize()+"("+arguments+")")
			require.NoError(t, err)
			var plan strings.Builder
			for rows.Next() {
				var line string
				require.NoError(t, rows.Scan(&line))
				plan.WriteString(line + "\n")
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			require.Contains(t, plan.String(), "idx_issues_uid_prefix")
			require.Contains(t, plan.String(), "uid ~>=~")
			require.Contains(t, plan.String(), "uid ~<~")
		}
	}
}
