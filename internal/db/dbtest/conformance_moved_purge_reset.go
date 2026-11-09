package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkMovedIssueOrdinaryPurgeResets(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	projects := make([]db.Project, 4)
	for i, name := range []string{"ordinary-source", "ordinary-middle", "ordinary-target", "ordinary-unrelated"} {
		var err error
		projects[i], err = store.CreateProject(ctx, name)
		require.NoError(t, err)
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Cached before departure", Author: "worker"})
	require.NoError(t, err)
	beforeMove, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	for i := range 2 {
		moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[i].ID, ToProjectID: projects[i+1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		issue = moved.Issue
	}
	purged, err := store.PurgeIssue(ctx, issue.ID, "worker", nil)
	require.NoError(t, err)
	require.NotNil(t, purged.PurgeResetAfterEventID)
	cursor := *purged.PurgeResetAfterEventID
	for i, project := range projects {
		t.Run(project.Name, func(t *testing.T) {
			reset, err := store.PurgeResetCheck(ctx, beforeMove, project.ID)
			require.NoError(t, err)
			if i == 3 {
				require.Zero(t, reset, "unrelated project is not invalidated")
				return
			}
			require.Equal(t, cursor, reset, "ordinary clients must reset after their departure history disappears")
			reset, err = store.PurgeResetCheck(ctx, cursor, project.ID)
			require.NoError(t, err)
			require.Zero(t, reset, "a client past the reset need not repeat it")
		})
	}
	reset, err := store.PurgeResetCheck(ctx, beforeMove, 0)
	require.NoError(t, err)
	require.Equal(t, cursor, reset)
	return nil
}
