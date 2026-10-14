package dbtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// checkRestrictedSearchReads proves that search never returns an issue from a
// project outside the caller's project boundary, with filters bound before
// and after that boundary so every placeholder lands on its own value.
func checkRestrictedSearchReads(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	visible, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	hidden, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	create := func(projectID int64, title string, labels ...string) db.Issue {
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projectID, Title: title, Author: "tester"})
		require.NoError(t, err)
		_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Body: "runnerrefused", Author: "tester"})
		require.NoError(t, err)
		for _, label := range labels {
			_, err := store.AddLabel(ctx, issue.ID, label, "tester")
			require.NoError(t, err)
		}
		return issue
	}
	visibleIssue := create(visible.ID, "visible task", "bug", "urgent")
	hiddenIssue := create(hidden.ID, "hidden task", "bug", "urgent")
	create(hidden.ID, "hidden excluded task", "bug", "urgent", "blocked")
	params := func(projectID int64) db.SearchFTSParams {
		return db.SearchFTSParams{ProjectID: projectID, Query: "runnerrefused", Status: "open", Limit: 20, Labels: []string{"bug", "urgent"}, ExcludeLabels: []string{"blocked"}}
	}
	onlyVisible := db.WithAuthorizedProjects(ctx, []string{visible.UID})
	onlyHidden := db.WithAuthorizedProjects(ctx, []string{hidden.UID})
	for name, search := range map[string]func(context.Context, db.SearchFTSParams) ([]db.SearchCandidate, error){"all": store.SearchFTS, "any": store.SearchFTSAny} {
		t.Run(name, func(t *testing.T) {
			hits, err := search(ctx, params(hidden.ID))
			require.NoError(t, err)
			assert.Equal(t, []int64{hiddenIssue.ID}, searchCandidateIDs(hits), "unrestricted control")

			hits, err = search(onlyVisible, params(hidden.ID))
			require.NoError(t, err)
			assert.Empty(t, searchCandidateIDs(hits), "a hidden project's issue never surfaces")
			hits, err = search(onlyHidden, params(visible.ID))
			require.NoError(t, err)
			assert.Empty(t, searchCandidateIDs(hits))
			hits, err = search(onlyVisible, params(visible.ID))
			require.NoError(t, err)
			assert.Equal(t, []int64{visibleIssue.ID}, searchCandidateIDs(hits), "the admitted project still matches")

			scoped := params(visible.ID)
			scoped.AllowedIssueIDs = []int64{visibleIssue.ID}
			scoped.IssueScope = &db.APITokenScope{RootIssueUID: visibleIssue.UID, ProjectUID: visible.UID}
			hits, err = search(onlyVisible, scoped)
			require.NoError(t, err)
			assert.Equal(t, []int64{visibleIssue.ID}, searchCandidateIDs(hits), "values bound after the boundary keep their positions")
			scoped.IssueScope = &db.APITokenScope{RootIssueUID: hiddenIssue.UID, ProjectUID: hidden.UID}
			hits, err = search(onlyVisible, scoped)
			require.NoError(t, err)
			assert.Empty(t, searchCandidateIDs(hits), "a scope rooted in a hidden project admits nothing")
		})
	}
	return nil
}
