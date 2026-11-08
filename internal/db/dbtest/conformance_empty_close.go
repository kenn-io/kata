package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Federation normalizes an omitted event payload to the same empty object on
// both stores before classifying or persisting a close.
func checkFederationEmptyClosePayload(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "empty-close-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "example", Author: "worker"})
	require.NoError(t, err)
	spoke, err := uid.New()
	require.NoError(t, err)
	event := newRemoteEvent(t, project, &issue.UID, "issue.closed", "worker", spoke, 9_000_000_000_000, jsontext.Value(`{}`))
	event.Payload = nil
	result, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: spoke, BoundActor: "worker", Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
	require.NoError(t, err)
	require.Equal(t, 1, result.Accepted)
	stored, err := store.EventsByUIDs(ctx, project.ID, []string{event.EventUID})
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.JSONEq(t, `{}`, stored[0].Payload)
	return nil
}
