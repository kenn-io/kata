package pgstore_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestIssueUIDPrefixGrowth(t *testing.T) {
	var large *pgstore.Store
	var stores []*pgstore.Store
	dbtest.RunIssueUIDPrefixGrowth(t, func(t *testing.T) db.Storage {
		store, _ := openConformanceStore(t)
		// Prepared statements and block counts belong to one session.
		store.SetMaxOpenConns(1)
		store.SetMaxIdleConns(1)
		t.Cleanup(func() { _ = store.Close() })
		stores = append(stores, store)
		return store
	}, func(t *testing.T, store db.Storage, size, offset int, prefix string, evenProject, oddProject int64) {
		postgres := store.(*pgstore.Store)
		if size == 100_000 {
			large = postgres
		}
		_, err := postgres.ExecContext(t.Context(), `INSERT INTO issues(uid,short_id,project_id,title,author)
SELECT $2||lpad(x::text,22,'0'),lpad(x::text,6,'0'),CASE x%2 WHEN 0 THEN $3::bigint ELSE $4::bigint END,'Unrelated issue','agent' FROM generate_series(1+$5::integer,$1+$5::integer) n(x)`, size, prefix, evenProject, oddProject, offset)
		require.NoError(t, err)
		_, err = postgres.ExecContext(t.Context(), `ANALYZE issues`)
		require.NoError(t, err)
	}, postgresBlockFetches(t, func() []*pgstore.Store { return stores }), 1)

	ctx := t.Context()
	project, err := large.ProjectByName(ctx, "example-project")
	require.NoError(t, err)
	second, err := large.ProjectByName(ctx, "second-project")
	require.NoError(t, err)
	// Inspect the cached queries after PostgreSQL can choose generic plans.
	rows, err := large.QueryContext(ctx, `SELECT name, cardinality(parameter_types) FROM pg_prepared_statements
WHERE statement LIKE '%uid LIKE %' AND statement NOT LIKE '%pg_prepared_statements%'`)
	require.NoError(t, err)
	statements := make(map[string]int)
	for rows.Next() {
		var name string
		var parameters int
		require.NoError(t, rows.Scan(&name, &parameters))
		switch parameters {
		case 2, 4, 5:
			statements[name] = parameters
		}
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.NotEmpty(t, statements, "expected cached UID prefix queries")
	for name, parameters := range statements {
		for _, prefix := range []string{"01ZX%", "01ZP%"} {
			arguments := []string{fmt.Sprintf("'%s'", prefix)}
			if parameters > 2 {
				arguments = append(arguments, "1", fmt.Sprintf("'%s'", project.UID))
				if parameters == 5 {
					arguments = append(arguments, fmt.Sprintf("'%s'", second.UID))
				}
			}
			arguments = append(arguments, "1")
			rows, err := large.QueryContext(ctx, "EXPLAIN EXECUTE "+pgx.Identifier{name}.Sanitize()+"("+strings.Join(arguments, ",")+")")
			require.NoError(t, err)
			var plan strings.Builder
			for rows.Next() {
				var line string
				require.NoError(t, rows.Scan(&line))
				plan.WriteString(line + "\n")
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			require.Regexp(t, `Index Cond: .*uid (?:~>=~|>=) `, plan.String())
			require.Regexp(t, `Index Cond: .*uid (?:~<~|<) `, plan.String())
		}
	}
	// PostgreSQL's query contract stays case-sensitive and uses byte order in every locale.
	for _, row := range []struct {
		uid       string
		projectID int64
	}{
		{"02hznq7v000000000000000001", project.ID},
		{"02HZNQ7V000000000000000003", project.ID},
		{"02HZNQ7V000000000000000000", second.ID},
	} {
		_, _, err = large.CreateIssue(ctx, db.CreateIssueParams{ProjectID: row.projectID, UID: row.uid, Title: "Prefix match", Author: "agent"})
		require.NoError(t, err)
	}
	matches, err := large.IssueUIDPrefixMatch(ctx, "02hznq7v", 20, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Equal(t, "02hznq7v000000000000000001", matches[0].UID)
	matches, err = large.IssueUIDPrefixMatch(db.WithAuthorizedProjects(ctx, []string{second.UID}), "02hznq7v", 20, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Empty(t, matches)
	for _, projects := range [][]string{nil, {project.UID}} {
		matches, err = large.IssueUIDPrefixMatch(db.WithAuthorizedProjects(ctx, projects), "02", 20, db.IncludeDeletedNo)
		require.NoError(t, err)
		expected := []string{"02HZNQ7V000000000000000000", "02HZNQ7V000000000000000003", "02hznq7v000000000000000001"}
		if projects != nil {
			expected = expected[1:]
		}
		require.Len(t, matches, len(expected))
		for i, uid := range expected {
			require.Equal(t, uid, matches[i].UID)
		}
	}
}
