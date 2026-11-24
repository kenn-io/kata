package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCloseRemovesNeedsReview(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, err := createFixtureIssue(ctx, store, project.ID, "Example issue", "worker", nil)
	require.NoError(t, err)
	for _, label := range []string{"needs-review", "bug"} {
		_, _, err := store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{Label: label, EventType: "issue.labeled", Actor: "worker"})
		require.NoError(t, err)
	}
	_, events, _, err := store.CloseIssueWithEvents(ctx, issue.ID, "done", "closer", "", nil)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "issue.closed", events[0].Type)
	require.Equal(t, "issue.unlabeled", events[1].Type)
	require.Equal(t, "closer", events[1].Actor)
	present, err := store.HasLabel(ctx, issue.ID, "needs-review")
	require.NoError(t, err)
	require.False(t, present)
	present, err = store.HasLabel(ctx, issue.ID, "bug")
	require.NoError(t, err)
	require.True(t, present)
	history, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, IssueUID: issue.UID, Limit: 100})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(history), 2)
	require.Equal(t, events[0].UID, history[len(history)-2].UID)
	require.Equal(t, events[1].UID, history[len(history)-1].UID)
	_, _, _, err = store.ReopenIssue(ctx, issue.ID, "worker")
	require.NoError(t, err)
	present, err = store.HasLabel(ctx, issue.ID, "needs-review")
	require.NoError(t, err)
	require.False(t, present)
	return nil
}
