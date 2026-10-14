package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkMoveReturnsCommittedEvent(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "move-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "move-target")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: source.ID, Title: "Move event", Author: "worker",
	})
	require.NoError(t, err)

	input := db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID,
		IfMatchRev: issue.Revision, Actor: "worker",
	}
	preview, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: input.IssueID, FromProjectID: input.FromProjectID, ToProjectID: input.ToProjectID,
		IfMatchRev: input.IfMatchRev, Actor: input.Actor, DryRun: true,
	})
	require.NoError(t, err)
	require.Nil(t, preview.Event, "a dry run must not return an uncommitted event")

	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	moved, err := store.MoveIssueProject(ctx, input)
	require.NoError(t, err)
	event := moved.Event
	require.NotNil(t, event)
	require.Equal(t, moved.EventID, event.ID)
	require.Equal(t, "issue.moved", event.Type)
	require.Equal(t, "worker", event.Actor)
	require.Equal(t, issue.UID, *event.IssueUID)

	committed, err := store.EventsAfter(ctx, db.EventsAfterParams{
		AfterID: before, ProjectID: target.ID, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, committed, 1)
	require.Equal(t, *event, committed[0], "the returned event must be the committed event")
	return nil
}
