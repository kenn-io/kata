package federation

import (
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

func TestMoveHistoryCreationPageSurvivesTransit404(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "source-project", "transit-project", "target-project")
	spoke := newMoveDeliverySpoke(t, hub, projects[1:2])
	issue, creation, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "History", Author: "worker"})
	require.NoError(t, err)
	envelope := func(e db.Event) api.EventEnvelope {
		return api.EventEnvelope{EventID: e.ID, EventUID: e.UID, OriginInstanceUID: e.OriginInstanceUID, ProjectID: e.ProjectID, ProjectUID: e.ProjectUID, ProjectName: e.ProjectName, IssueUID: e.IssueUID, RelatedIssueUID: e.RelatedIssueUID, Type: e.Type, Actor: e.Actor, HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, ContentHash: e.ContentHash, Payload: jsontext.Value(e.Payload), CreatedAt: e.CreatedAt}
	}
	_, err = spoke.env.DB.InsertRemoteEvents(ctx, spoke.projects[0].ID, []db.RemoteEvent{remoteEventFromEnvelope(envelope(creation))})
	require.NoError(t, err)
	retained, err := spoke.env.DB.ProjectByUID(ctx, projects[0].UID)
	require.NoError(t, err)
	_, err = spoke.env.DB.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: retained.ID, Actor: "worker"})
	require.ErrorIs(t, err, db.ErrFederationProjectIdentityRequired)
	inbound, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	outbound, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[1].ID, ToProjectID: projects[2].ID, IfMatchRev: inbound.Issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }))
	defer server.Close()
	client, err := NewClient(ctx, server.URL, "token", clientpkg.Opts{})
	require.NoError(t, err)
	page := api.PollEventsBody{Events: []api.EventEnvelope{envelope(*inbound.Event), envelope(*outbound.Event)}, NextAfterID: outbound.Event.ID}
	events, err := backfillFederationMoveHistory(ctx, client, projects[1].ID, page)
	require.NoError(t, err)
	require.Len(t, events, 2)
	batch := make([]db.RemoteEvent, len(events))
	for i, e := range events {
		batch[i] = remoteEventFromEnvelope(e)
	}
	_, err = spoke.env.DB.InsertRemoteEvents(ctx, spoke.projects[0].ID, batch)
	require.NoError(t, err)
	require.NoError(t, spoke.env.DB.MaterializeFederatedProject(ctx, spoke.projects[0].ID))
	history, err := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 100})
	require.NoError(t, err)
	found := false
	for _, e := range history {
		if e.UID == creation.UID && e.ContentHash == creation.ContentHash && e.Actor == creation.Actor {
			found = true
		}
	}
	require.True(t, found, "original creation envelope survives the transit 404")
}
