package sqlitestore_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// TestListIssueContentPaginatesLiveIssues verifies that ListIssueContent pages
// through live issues in id order, skipping soft-deleted rows.
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
	// Soft-delete the middle issue: it must not be listed.
	_, _, _, err := d.SoftDeleteIssue(ctx, issues[1].ID, "x")
	require.NoError(t, err)

	page1, err := d.ListIssueContent(ctx, 0, 1)
	require.NoError(t, err)
	require.Lenf(t, page1, 1, "page1 = %+v", page1)
	require.Equal(t, uids[0], page1[0].UID)
	require.Equal(t, proj.UID, page1[0].ProjectUID)

	page2, err := d.ListIssueContent(ctx, page1[0].ID, 10)
	require.NoError(t, err)
	require.Lenf(t, page2, 1, "page2 must skip deleted issue, got %+v", page2)
	require.Equal(t, uids[2], page2[0].UID)
}
