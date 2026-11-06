package sqlitestore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestIssueUIDPrefixGrowth(t *testing.T) {
	var stores []*sql.DB
	pages := func(reset bool) int {
		total := 0
		for _, store := range stores {
			conn, err := store.Conn(t.Context())
			require.NoError(t, err)
			total += sqlitePageFetches(t, conn, reset)
			require.NoError(t, conn.Close())
		}
		return total
	}
	dbtest.RunIssueUIDPrefixGrowth(t, func(t *testing.T) db.Storage {
		store, path := openTestDBWithPath(t)
		// The write-queue wrapper doesn't expose the driver's page counters.
		raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, raw.Close()) })
		raw.SetMaxOpenConns(1)
		stores = append(stores, raw)
		measured := *store
		measured.DB = raw
		return &measured
	}, func(t *testing.T, store db.Storage, size, offset int, prefix string, evenProject, oddProject int64) {
		_, err := store.(*sqlitestore.Store).ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (
    VALUES(1+?) UNION ALL SELECT x+1 FROM n WHERE x<?
)
INSERT INTO issues(uid,short_id,project_id,title,author)
SELECT ?||printf('%022d',x),printf('%06d',x),CASE x%2 WHEN 0 THEN ? ELSE ? END,'Unrelated issue','agent' FROM n`, offset, size+offset, prefix, evenProject, oddProject)
		require.NoError(t, err)
	}, func(run, _ func()) float64 {
		// Count database work instead of elapsed time, which varies with CI load.
		pages(true)
		run()
		return float64(pages(false))
	}, 1)
}

func TestIssueUIDPrefixMatchCaseMatching(t *testing.T) {
	store := openTestDB(t)
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, UID: "01hznq7v000000000000000001", Title: "Prefix match", Author: "agent"})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, UID: "01HZNQ7V000000000000000003", Title: "Other match", Author: "agent"})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, UID: "01HZNQ7V000000000000000001", Title: "Case tie", Author: "agent"})
	require.NoError(t, err)
	for _, projects := range [][]string{nil, {project.UID}} {
		for _, prefix := range []string{"01HZNQ7V", "01hznq7v"} {
			matches, err := store.IssueUIDPrefixMatch(db.WithAuthorizedProjects(t.Context(), projects), prefix, 20, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Len(t, matches, 3)
			// SQLite deliberately matches without case and orders ties by original bytes.
			require.Equal(t, "01HZNQ7V000000000000000001", matches[0].UID)
			require.Equal(t, "01hznq7v000000000000000001", matches[1].UID)
			require.Equal(t, "01HZNQ7V000000000000000003", matches[2].UID)
		}
	}
}
