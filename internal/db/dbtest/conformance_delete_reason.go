package dbtest

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A hub keeps the reason a spoke gave for a delete, so restorers reading the
// hub's history see it too.
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
	_, deleted, changed, err := spoke.SoftDeleteIssue(ctx, spokeIssue.ID, "tester", "duplicate of the import task")
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, deleted)

	accepted, err := hub.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hubProject.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "tester",
		Events: []db.FederationIngestEvent{{SourceEventID: deleted.ID, Event: remoteEventFromStored(*deleted)}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, accepted.Accepted)
	hubDeleted, err := hub.IssueByID(ctx, hubIssue.ID)
	require.NoError(t, err)
	assert.NotNil(t, hubDeleted.DeletedAt)
	events, err := hub.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: hubProject.ID, IssueUID: hubIssue.UID, Types: []string{"issue.soft_deleted"}, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	var payload struct {
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
	assert.Equal(t, "duplicate of the import task", payload.Reason)
	return nil
}
