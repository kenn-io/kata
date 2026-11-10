package sqlitestore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestFederationReplayCostUnchangedIssues(t *testing.T) {
	var fetchCounts []int
	for _, count := range []int{8, 64} {
		d, ctx, p := setupTestProject(t)
		_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "work", Author: "tester"})
		require.NoError(t, err)
		_, err = d.EnableProjectFederation(ctx, p.ID, "tester")
		require.NoError(t, err)
		for range count {
			issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "work", Author: "tester"})
			require.NoError(t, err)
			_, _, err = d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Body: "note", Author: "tester"})
			require.NoError(t, err)
		}
		upsertTestSpokeFederationBinding(ctx, t, d, p, true)
		raw, err := sql.Open("sqlite", d.Path()+"?_pragma=foreign_keys(1)")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, raw.Close()) })
		raw.SetMaxOpenConns(1)
		store := &sqlitestore.Store{DB: raw}
		require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
		pages := func(reset bool) int {
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			defer func() { require.NoError(t, conn.Close()) }()
			return sqlitePageFetches(t, conn, reset)
		}
		pages(true)
		require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
		fetches := pages(false)
		t.Logf("%d unchanged issues: %d page fetches", count+1, fetches)
		fetchCounts = append(fetchCounts, fetches)
	}
	// Full replay still scans history and joins comments to issues.
	// Allow those reads while catching per-issue updates and comment ownership checks.
	require.LessOrEqual(t, fetchCounts[1], fetchCounts[0]+300)
}
