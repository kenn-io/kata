package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkMovedOrdinaryIssueAdoption(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: source.ID, Title: "Moved ordinary issue", Author: "operator",
	})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID,
		IfMatchRev: issue.Revision, Actor: "operator",
	})
	require.NoError(t, err)
	hubUID, err := uid.New()
	require.NoError(t, err)
	adopted, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID: destination.ID, HubURL: "https://daemon.example", HubProjectID: 42,
		HubProjectUID: hubUID, ReplayHorizonEventID: 1, Actor: "operator",
	})
	require.NoError(t, err, "ordinary move history can be replaced by an adoption snapshot")
	retained, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, destination.ID, retained.ProjectID)
	require.Equal(t, issue.Title, retained.Title)
	events, err := store.PendingFederationPushEvents(ctx, destination.ID, store.InstanceUID(), adopted.Binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "issue.snapshot", events[0].Type)
	require.Equal(t, hubUID, events[0].ProjectUID)
	require.Equal(t, &issue.UID, events[0].IssueUID)
	return nil
}
