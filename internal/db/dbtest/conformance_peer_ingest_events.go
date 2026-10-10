package dbtest

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkPeerIngestCommittedEvents(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	peer, err := store.CreateProject(ctx, "peer-project")
	require.NoError(t, err)
	first, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "First", Author: "worker"})
	require.NoError(t, err)
	second, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: peer.ID, Title: "Second", Author: "worker"})
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, source.ID, "worker")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, peer.ID, "worker")
	require.NoError(t, err)
	holder, err := uid.New()
	require.NoError(t, err)
	_, err = store.AcquireClaim(ctx, db.AcquireClaimParams{ProjectID: peer.ID, IssueRef: second.ShortID, Principal: db.ClaimPrincipal{HolderInstanceUID: holder, Holder: "holder"}, ClaimKind: "hard", Now: time.Now().UTC()})
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	eventUID, err := uid.New()
	require.NoError(t, err)
	payload, err := json.Marshal(map[string]string{"issue_uid": first.UID, "from_uid": first.UID, "to_uid": second.UID, "type": "blocks"})
	require.NoError(t, err)
	event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: source.UID, ProjectName: source.Name, IssueUID: &first.UID, RelatedIssueUID: &second.UID, Type: "issue.linked", Actor: "worker", HLCPhysicalMS: 100, CreatedAt: time.Now().UTC().Truncate(time.Millisecond), Payload: payload}
	resignNativeEvent(t, &event)
	input := db.FederationIngestParams{ProjectID: source.ID, SpokeInstanceUID: origin, BoundActor: "worker", Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}}
	result, err := store.IngestFederationEvents(ctx, input)
	require.NoError(t, err)
	require.Equal(t, 1, result.Accepted)
	require.Len(t, result.Events, 2)
	require.Len(t, result.InsertedEventUIDs, 2)
	require.Equal(t, eventUID, result.Events[0].UID)
	require.Equal(t, source.ID, result.Events[0].ProjectID)
	require.Equal(t, "claim.violated", result.Events[1].Type)
	require.Equal(t, peer.ID, result.Events[1].ProjectID)
	require.Equal(t, second.UID, *result.Events[1].IssueUID)
	require.NotEqual(t, result.InsertedEventUIDs[0], result.InsertedEventUIDs[1])
	repeated, err := store.IngestFederationEvents(ctx, input)
	require.NoError(t, err)
	require.Equal(t, 1, repeated.Duplicates)
	require.Empty(t, repeated.Events)
	return nil
}
