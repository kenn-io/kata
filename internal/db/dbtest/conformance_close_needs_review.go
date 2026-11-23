package dbtest

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCloseRemovesNeedsReview(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, err := createFixtureIssue(ctx, store, project.ID, "Example issue", "worker", nil)
	require.NoError(t, err)
	for _, label := range []string{"needs-review", "bug"} {
		_, _, err = store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{Label: label, EventType: "issue.labeled", Actor: "worker"})
		require.NoError(t, err)
	}
	_, events, changed, err := store.CloseIssueWithEvents(ctx, issue.ID, "done", "closer", "", nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, events, 2)
	require.Equal(t, "issue.closed", events[0].Type)
	require.Equal(t, "issue.unlabeled", events[1].Type)
	require.Equal(t, "closer", events[1].Actor)
	var payload struct {
		Label string `json:"label"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[1].Payload), &payload))
	require.Equal(t, "needs-review", payload.Label)
	present, err := store.HasLabel(ctx, issue.ID, "needs-review")
	require.NoError(t, err)
	require.False(t, present)
	present, err = store.HasLabel(ctx, issue.ID, "bug")
	require.NoError(t, err)
	require.True(t, present)
	_, _, _, err = store.ReopenIssue(ctx, issue.ID, "worker")
	require.NoError(t, err)
	present, err = store.HasLabel(ctx, issue.ID, "needs-review")
	require.NoError(t, err)
	require.False(t, present)
	_, events, changed, err = store.CloseIssueWithEvents(ctx, issue.ID, "wontfix", "closer", "", nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, events, 2)
	require.Equal(t, "issue.closed", events[0].Type)
	require.Equal(t, "issue.unlabeled", events[1].Type)
	return nil
}

func checkCloseRemovesDelayedNeedsReview(t *testing.T, spoke db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := spoke.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "worker", PushEnabled: true, Enabled: true,
	})
	require.NoError(t, err)
	issue, created, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Example issue", Author: "worker"})
	require.NoError(t, err)
	hub := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, hub.Close()) })
	hubProject, err := hub.CreateProjectWithUID(ctx, project.Name, project.UID)
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, hubProject.ID, "operator")
	require.NoError(t, err)
	_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hubProject.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker",
		Events: []db.FederationIngestEvent{{SourceEventID: created.ID, Event: remoteEventFromStored(created)}},
	})
	require.NoError(t, err)
	hubIssue, err := hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, labeled, err := hub.AddLabelAndEvent(ctx, hubIssue.ID, db.LabelEventParams{Label: "needs-review", EventType: "issue.labeled", Actor: "reviewer"})
	require.NoError(t, err)
	// The label precedes the close in the federation clock, but arrives late.
	//nolint:kennlint // sleeptest: separate the replicas' real millisecond clocks.
	time.Sleep(time.Until(time.UnixMilli(labeled.HLCPhysicalMS + 1)))
	_, events, changed, err := spoke.CloseIssueWithEvents(ctx, issue.ID, "done", "worker", "", nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, events, 2)
	_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hubProject.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker",
		Events: []db.FederationIngestEvent{{SourceEventID: events[0].ID, Event: remoteEventFromStored(events[0])}, {SourceEventID: events[1].ID, Event: remoteEventFromStored(events[1])}},
	})
	require.NoError(t, err)
	present, err := hub.HasLabel(ctx, hubIssue.ID, "needs-review")
	require.NoError(t, err)
	require.False(t, present)
	_, err = spoke.InsertRemoteEvent(ctx, project.ID, remoteEventFromStored(labeled))
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, project.ID))
	issue, err = spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	present, err = spoke.HasLabel(ctx, issue.ID, "needs-review")
	require.NoError(t, err)
	require.False(t, present)
	return nil
}
