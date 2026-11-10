package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedMovePayloadOnlyHistory(t *testing.T, hub db.Storage, backend Backend) error {
	for _, identityField := range []string{"issue_uid", "uid"} {
		t.Run(identityField, func(t *testing.T) {
			ctx := t.Context()
			source, err := hub.CreateProject(ctx, "payload-history-source-"+identityField)
			require.NoError(t, err)
			target, err := hub.CreateProject(ctx, "payload-history-target-"+identityField)
			require.NoError(t, err)
			for _, project := range []db.Project{source, target} {
				_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
				require.NoError(t, err)
			}
			issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: source.ID, Title: "creation title", Author: "worker",
			})
			require.NoError(t, err)
			acceptedTitle := "accepted " + identityField + " edit"
			_, edit, changed, err := hub.EditIssue(ctx, db.EditIssueParams{
				IssueID: issue.ID, Title: &acceptedTitle, Actor: "worker",
			})
			require.NoError(t, err)
			require.True(t, changed)
			require.NotNil(t, edit)

			payload := db.PayloadMap(jsontext.Value(edit.Payload))
			delete(payload, "issue_uid")
			delete(payload, "uid")
			payload[identityField] = jsontext.Value(`"` + issue.UID + `"`)
			payload["title"] = jsontext.Value(`"` + acceptedTitle + `"`)
			rawPayload, err := json.Marshal(payload)
			require.NoError(t, err)
			payloadOnlyEdit := remoteEventFromStored(*edit)
			payloadOnlyEdit.EventUID, err = uid.New()
			require.NoError(t, err)
			payloadOnlyEdit.OriginInstanceUID, err = uid.New()
			require.NoError(t, err)
			payloadOnlyEdit.IssueUID = nil
			payloadOnlyEdit.HLCCounter++
			payloadOnlyEdit.Payload = jsontext.Value(rawPayload)
			payloadOnlyEdit.ContentHash = remoteEventHash(t, payloadOnlyEdit)
			_, err = hub.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{payloadOnlyEdit})
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
			retained, err := hub.EventsByUIDs(ctx, source.ID, []string{payloadOnlyEdit.EventUID})
			require.NoError(t, err)
			require.Len(t, retained, 1, "source reset must retain payload-only history for the moved issue")
			require.Equal(t, payloadOnlyEdit.ContentHash, retained[0].ContentHash)

			feed, err := hub.ReadFederation(ctx, db.FederationReadParams{
				ProjectID: target.ID, IssueUID: issue.UID, Limit: 100,
				Features: db.SupportedEventFeatures,
			})
			require.NoError(t, err)
			foundPayloadOnlyEdit := false
			for _, event := range feed.Events {
				foundPayloadOnlyEdit = foundPayloadOnlyEdit || event.UID == payloadOnlyEdit.EventUID
			}
			require.True(t, foundPayloadOnlyEdit,
				"destination move history must include accepted issue updates identified by payload issue_uid or uid")

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
			_, err = replica.InsertRemoteEvent(ctx, localTarget.ID, payloadOnlyEdit)
			require.NoError(t, err)
			_, err = replica.InsertRemoteEvent(ctx, localTarget.ID, remoteEventFromStored(*moved.Event))
			require.NoError(t, err)
			require.NoError(t, replica.MaterializeFederatedProject(ctx, localTarget.ID))

			got, err := replica.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, localTarget.ID, got.ProjectID)
			require.Equal(t, acceptedTitle, got.Title,
				"destination replay must include edits identified by payload issue_uid or uid")
		})
	}
	return nil
}
