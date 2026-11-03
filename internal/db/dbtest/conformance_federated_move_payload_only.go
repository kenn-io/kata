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
			updated, edit, changed, err := hub.EditIssue(ctx, db.EditIssueParams{
				IssueID: issue.ID, Title: &acceptedTitle, Actor: "worker",
			})
			require.NoError(t, err)
			require.True(t, changed)
			require.NotNil(t, edit)
			moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{
				IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID,
				IfMatchRev: updated.Revision, Actor: "worker",
			})
			require.NoError(t, err)
			require.NotNil(t, moved.Event)

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
			payloadOnlyEdit.IssueUID = nil
			payloadOnlyEdit.Payload = jsontext.Value(rawPayload)
			payloadOnlyEdit.ContentHash = remoteEventHash(t, payloadOnlyEdit)

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
			_, err = replica.InsertRemoteEvents(ctx, localSource.ID, []db.RemoteEvent{
				remoteEventFromStored(created), payloadOnlyEdit,
			})
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
