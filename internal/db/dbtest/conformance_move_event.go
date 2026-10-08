package dbtest

import (
	"reflect"
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
	require.Nil(t, moveResultEvent(t, preview), "a dry run must not return an uncommitted event")

	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	moved, err := store.MoveIssueProject(ctx, input)
	require.NoError(t, err)
	event := moveResultEvent(t, moved)
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

func moveResultEvent(t *testing.T, result db.MoveIssueProjectOut) *db.Event {
	t.Helper()
	field := reflect.ValueOf(result).FieldByName("Event")
	require.True(t, field.IsValid(), "MoveIssueProjectOut must return its committed event")
	require.Equal(t, reflect.TypeOf((*db.Event)(nil)), field.Type(), "Event must be optional for dry-run previews")
	if field.IsNil() {
		return nil
	}
	event, ok := field.Interface().(*db.Event)
	require.True(t, ok)
	return event
}
