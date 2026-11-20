package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A hub keeps the reason a spoke gave for a delete, so restorers reading the
// hub's history see it too. A delete without a reason keeps the payload older
// peers already receive.
func checkSoftDeleteReasonFederates(t *testing.T, hub db.Storage, backend Backend) error {
	ctx := t.Context()
	hubProject, err := hub.CreateProject(ctx, "example-delete-reason-hub")
	require.NoError(t, err)
	hubIssue, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID, Title: "Duplicate report", Body: "Same as the import task", Author: "tester",
	})
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, hubProject.ID, "tester")
	require.NoError(t, err)

	spoke := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, spoke.Close()) })
	spokeProject, err := spoke.CreateProjectWithUID(ctx, "example-delete-reason-spoke", hubProject.UID)
	require.NoError(t, err)
	spokeIssue, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spokeProject.ID, UID: hubIssue.UID, Title: hubIssue.Title, Body: hubIssue.Body, Author: "tester",
	})
	require.NoError(t, err)
	_, deleteEvent, changed, err := spoke.SoftDeleteIssue(ctx, spokeIssue.ID, "tester", "duplicate of the import task")
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, deleteEvent)

	accepted, err := hub.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hubProject.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "tester",
		Events: []db.FederationIngestEvent{{SourceEventID: deleteEvent.ID, Event: remoteEventFromStored(*deleteEvent)}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, accepted.Accepted)
	hubDeleted, err := hub.IssueByID(ctx, hubIssue.ID)
	require.NoError(t, err)
	assert.NotNil(t, hubDeleted.DeletedAt)
	assert.JSONEq(t, `"duplicate of the import task"`, string(softDeletePayload(t, hub, hubProject.ID, hubIssue.UID)["reason"]))

	plain, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hubProject.ID, Title: "Stale note", Author: "tester"})
	require.NoError(t, err)
	_, _, changed, err = hub.SoftDeleteIssue(ctx, plain.ID, "tester", "")
	require.NoError(t, err)
	require.True(t, changed)
	payload := softDeletePayload(t, hub, hubProject.ID, plain.UID)
	assert.Len(t, payload, 1, "a delete without a reason writes only deleted_at: %v", payload)
	assert.Contains(t, payload, "deleted_at")
	return nil
}

func softDeletePayload(t *testing.T, store db.Storage, projectID int64, issueUID string) map[string]jsontext.Value {
	t.Helper()
	events, err := store.EventsAfter(t.Context(), db.EventsAfterParams{
		ProjectID: projectID, IssueUID: issueUID, Types: []string{"issue.soft_deleted"}, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	var payload map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
	return payload
}
