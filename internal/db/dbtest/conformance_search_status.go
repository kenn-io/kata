package dbtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkSearchStatus(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	sibling, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	create := func(projectID int64, title, status string, deleted bool, labels ...string) db.Issue {
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projectID, Title: title, Author: "tester"})
		require.NoError(t, err)
		_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Body: "runnerrefused", Author: "tester"})
		require.NoError(t, err)
		for _, label := range labels {
			_, err := store.AddLabel(ctx, issue.ID, label, "tester")
			require.NoError(t, err)
		}
		if status == "closed" {
			_, _, _, err := store.CloseIssue(ctx, issue.ID, "done", "tester", "", nil)
			require.NoError(t, err)
		}
		if deleted {
			_, _, _, err := store.SoftDeleteIssue(ctx, issue.ID, "tester")
			require.NoError(t, err)
		}
		return issue
	}
	open := create(project.ID, "current task", "open", false, "bug", "urgent")
	closed := create(project.ID, "historical task", "closed", false, "bug", "urgent")
	deletedOpen := create(project.ID, "deleted current task", "open", true, "bug", "urgent")
	deletedClosed := create(project.ID, "deleted historical task", "closed", true, "bug", "urgent")
	create(project.ID, "excluded task", "open", false, "bug", "urgent", "blocked")
	create(project.ID, "unlabeled task", "open", false)
	create(sibling.ID, "sibling task", "open", false, "bug", "urgent")
	for name, search := range map[string]func(context.Context, db.SearchFTSParams) ([]db.SearchCandidate, error){"all": store.SearchFTS, "any": store.SearchFTSAny} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				status  string
				deleted bool
				want    []int64
			}{
				{"open", false, []int64{open.ID}}, {"closed", false, []int64{closed.ID}},
				{"", false, []int64{open.ID, closed.ID}},
				{"open", true, []int64{open.ID, deletedOpen.ID}}, {"closed", true, []int64{closed.ID, deletedClosed.ID}},
				{"", true, []int64{open.ID, closed.ID, deletedOpen.ID, deletedClosed.ID}},
			} {
				hits, err := search(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: "runnerrefused", Status: tc.status, IncludeDeleted: tc.deleted, Limit: 20, Labels: []string{"BUG", "urgent"}, ExcludeLabels: []string{"BLOCKED"}})
				require.NoError(t, err)
				assert.ElementsMatch(t, tc.want, searchCandidateIDs(hits), "status=%q deleted=%v", tc.status, tc.deleted)
				for _, hit := range hits {
					assert.Equal(t, []string{"comments"}, hit.MatchedIn)
				}
			}
			// Choose the opposite status from the unfiltered first hit, so either
			// backend's ranking exposes filtering that happens after LIMIT.
			params := db.SearchFTSParams{ProjectID: project.ID, Query: "runnerrefused", Limit: 1, Labels: []string{"urgent"}, ExcludeLabels: []string{"blocked"}}
			first, err := search(ctx, params)
			require.NoError(t, err)
			require.Len(t, first, 1)
			params.Status = "open"
			want := open.ID
			if first[0].Issue.Status == "open" {
				params.Status = "closed"
				want = closed.ID
			}
			filtered, err := search(ctx, params)
			require.NoError(t, err)
			require.Len(t, filtered, 1)
			assert.Equal(t, want, filtered[0].Issue.ID)
		})
	}
	return nil
}
