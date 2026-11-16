package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// checkFederatedMoveRemoteEditHistory proves an edit the hub received from
// another instance, which it stores without a local issue row, stays part of
// the moved issue's history for the destination feed and its replicas.
func checkFederatedMoveRemoteEditHistory(t *testing.T, hub db.Storage, backend Backend) error {
	ctx := t.Context()
	source, err := hub.CreateProject(ctx, "remote-history-source")
	require.NoError(t, err)
	target, err := hub.CreateProject(ctx, "remote-history-target")
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: source.ID, Title: "creation title", Author: "worker",
	})
	require.NoError(t, err)
	acceptedTitle := "accepted remote edit"
	_, edit, changed, err := hub.EditIssue(ctx, db.EditIssueParams{
		IssueID: issue.ID, Title: &acceptedTitle, Actor: "worker",
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, edit)

	payload := db.PayloadMap(jsontext.Value(edit.Payload))
	payload["title"] = jsontext.Value(`"` + acceptedTitle + `"`)
	rawPayload, err := json.Marshal(payload)
	require.NoError(t, err)
	remoteEdit := remoteEventFromStored(*edit)
	remoteEdit.EventUID, err = uid.New()
	require.NoError(t, err)
	remoteEdit.OriginInstanceUID, err = uid.New()
	require.NoError(t, err)
	remoteEdit.HLCCounter++
	remoteEdit.Payload = jsontext.Value(rawPayload)
	remoteEdit.ContentHash = remoteEventHash(t, remoteEdit)
	_, err = hub.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{remoteEdit})
	require.NoError(t, err)
	require.NoError(t, hub.MaterializeFederatedProject(ctx, source.ID))
	issue, err = hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID,
		IfMatchRev: issue.Revision, Actor: "worker",
	})
	require.NoError(t, err)
	require.NotNil(t, moved.Event)
	prepareRehomePull(t, hub, source, target, "pull")
	cursor, err := hub.MaxEventID(ctx)
	require.NoError(t, err)
	require.NoError(t, hub.AdvanceFederationPushCursor(ctx, source.ID, cursor))
	require.NoError(t, hub.ResetFederatedProject(ctx, source.ID, cursor+1, cursor))
	retained, err := hub.EventsByUIDs(ctx, source.ID, []string{remoteEdit.EventUID})
	require.NoError(t, err)
	require.Len(t, retained, 1, "source reset must retain remote history for the moved issue")
	require.Equal(t, remoteEdit.ContentHash, retained[0].ContentHash)

	feed, err := hub.ReadFederation(ctx, db.FederationReadParams{
		ProjectID: target.ID, IssueUID: issue.UID, Limit: 100,
		Features: db.SupportedEventFeatures,
	})
	require.NoError(t, err)
	foundRemoteEdit := false
	for _, event := range feed.Events {
		foundRemoteEdit = foundRemoteEdit || event.UID == remoteEdit.EventUID
	}
	require.True(t, foundRemoteEdit,
		"destination move history must include accepted remote issue updates")

	replica := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, replica.Close()) })
	localSource, err := replica.CreateProjectWithUID(ctx, source.Name, source.UID)
	require.NoError(t, err)
	localTarget, err := replica.CreateProjectWithUID(ctx, target.Name, target.UID)
	require.NoError(t, err)
	for _, pair := range []struct {
		local db.Project
		hub   db.Project
	}{{localSource, source}, {localTarget, target}} {
		_, err := replica.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: pair.local.ID, Role: db.FederationRoleSpoke,
			HubURL: "https://hub.example", HubProjectID: pair.hub.ID,
			HubProjectUID: pair.hub.UID, Actor: "worker", Enabled: true, PushEnabled: true,
		})
		require.NoError(t, err)
	}
	_, err = replica.InsertRemoteEvent(ctx, localSource.ID, remoteEventFromStored(created))
	require.NoError(t, err)
	_, err = replica.InsertRemoteEvent(ctx, localTarget.ID, remoteEdit)
	require.NoError(t, err)
	_, err = replica.InsertRemoteEvent(ctx, localTarget.ID, remoteEventFromStored(*moved.Event))
	require.NoError(t, err)
	require.NoError(t, replica.MaterializeFederatedProject(ctx, localTarget.ID))

	got, err := replica.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, localTarget.ID, got.ProjectID)
	require.Equal(t, acceptedTitle, got.Title,
		"destination replay must include remote edits")
	return nil
}
