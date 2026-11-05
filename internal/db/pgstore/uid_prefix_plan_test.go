package pgstore_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestIssueUIDPrefixUsesPrefixIndex(t *testing.T) {
	store, _ := openConformanceStore(t)
	t.Cleanup(func() { _ = store.Close() })
	// pg_prepared_statements belongs to one session, so keep the calls on one connection.
	store.SetMaxOpenConns(1)
	store.SetMaxIdleConns(1)
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)

	_, err = store.ExecContext(ctx, `INSERT INTO issues(uid,short_id,project_id,title,author)
SELECT '01M4'||lpad(x::text,22,'0'),lpad(x::text,6,'0'),$1,'Unrelated issue','agent' FROM generate_series(1,100000) n(x)`, project.ID)
	require.NoError(t, err)
	// Put the match after unrelated UIDs so LIMIT cannot hide a full index scan.
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, UID: "01ZZ0000000000000000000001", Title: "Last match", Author: "agent"})
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `ANALYZE issues`)
	require.NoError(t, err)
	for _, projects := range [][]string{nil, {project.UID}} {
		lookupContext := db.WithAuthorizedProjects(ctx, projects)
		for range 12 {
			matches, err := store.IssueUIDPrefixMatch(lookupContext, "01ZZ", 1, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Len(t, matches, 1)
			require.Equal(t, "01ZZ0000000000000000000001", matches[0].UID)
			matches, err = store.IssueUIDPrefixMatch(lookupContext, "01ZY", 1, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Empty(t, matches)
		}
	}
	// Inspect the real pgx-cached queries after PostgreSQL can choose generic plans.
	rows, err := store.QueryContext(ctx, `SELECT name, cardinality(parameter_types) FROM pg_prepared_statements
WHERE statement LIKE '%uid LIKE %' AND statement NOT LIKE '%pg_prepared_statements%'`)
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
			if parameters == 4 {
				arguments = fmt.Sprintf("'%s',1,'%s',1", prefix, project.UID)
			}
			rows, err := store.QueryContext(ctx, "EXPLAIN EXECUTE "+pgx.Identifier{name}.Sanitize()+"("+arguments+")")
			require.NoError(t, err)
			var plan strings.Builder
			for rows.Next() {
				var line string
				require.NoError(t, rows.Scan(&line))
				plan.WriteString(line + "\n")
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			// C collation can seek the ordinary unique index with the same bounds.
			require.Regexp(t, `Index Cond: .*uid (?:~>=~|>=) `, plan.String())
			require.Regexp(t, `Index Cond: .*uid (?:~<~|<) `, plan.String())
		}
	}
}
