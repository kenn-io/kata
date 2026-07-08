package sqlitestore_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// TestListIssueContentPaginatesLiveIssues verifies that ListIssueContent pages
// through issues in id order, excluding soft-deleted issues (their content
// must never be sent to the embedding endpoint after deletion) and issues
// whose project is deleted. A restored issue rejoins the feed.
func TestListIssueContentPaginatesLiveIssues(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	proj := createProject(ctx, t, d, "spoke-project")

	var issues []db.Issue
	for i := 0; i < 3; i++ {
		iss, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: proj.ID, Title: fmt.Sprintf("t%d", i), Body: "b", Author: "x",
		})
		require.NoError(t, err)
		issues = append(issues, iss)
	}
	uids := []string{issues[0].UID, issues[1].UID, issues[2].UID}
	// Soft-delete the middle issue: it must leave the feed so its content is
	// never re-sent to the embedding endpoint while deleted.
	_, _, _, err := d.SoftDeleteIssue(ctx, issues[1].ID, "x")
	require.NoError(t, err)

	// An issue in a deleted project must not be listed.
	gone := createProject(ctx, t, d, "gone-project")
	_, _, err = d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: gone.ID, Title: "orphaned", Body: "b", Author: "x",
	})
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: gone.ID, Actor: "x", Force: true})
	require.NoError(t, err)

	page1, err := d.ListIssueContent(ctx, 0, 2)
	require.NoError(t, err)
	require.Lenf(t, page1, 2, "page1 = %+v", page1)
	require.Equal(t, uids[0], page1[0].UID)
	require.Equal(t, uids[2], page1[1].UID, "soft-deleted issue must leave the feed")
	require.Equal(t, proj.UID, page1[0].ProjectUID)

	page2, err := d.ListIssueContent(ctx, page1[1].ID, 10)
	require.NoError(t, err)
	require.Emptyf(t, page2, "page2 must exclude the deleted project's issue, got %+v", page2)

	// Restore: the issue rejoins the feed so it is re-mirrored and re-embedded.
	_, _, _, err = d.RestoreIssue(ctx, issues[1].ID, "x")
	require.NoError(t, err)
	all, err := d.ListIssueContent(ctx, 0, 10)
	require.NoError(t, err)
	require.Lenf(t, all, 3, "restored issue must rejoin the feed, got %+v", all)
	require.Equal(t, uids[1], all[1].UID)
}
