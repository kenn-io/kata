package dbtest

import (
	"encoding/json/v2"
	"testing"

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
		_, labeled, err := store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{Label: label, EventType: "issue.labeled", Actor: "worker"})
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(labeled.Payload), &payload))
		if label == "needs-review" {
			require.Contains(t, payload, "after_close_uids")
			require.Empty(t, payload["after_close_uids"])
		} else {
			require.NotContains(t, payload, "after_close_uids")
		}
	}
	_, events, changed, err := store.CloseIssueWithEvents(ctx, issue.ID, "done", "closer", "", nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, events, 2)
	require.Equal(t, "issue.closed", events[0].Type)
	require.Equal(t, "issue.unlabeled", events[1].Type)
	require.Equal(t, "closer", events[1].Actor)
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
	_, labeled, err := store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{Label: "needs-review", EventType: "issue.labeled", Actor: "worker"})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(labeled.Payload), &payload))
	require.Equal(t, []any{events[0].UID}, payload["after_close_uids"])
	firstClose := events[0].UID
	_, events, changed, err = store.CloseIssueWithEvents(ctx, issue.ID, "wontfix", "closer", "", nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, events, 2)
	require.Equal(t, "issue.closed", events[0].Type)
	require.Equal(t, "issue.unlabeled", events[1].Type)
	_, _, _, err = store.ReopenIssue(ctx, issue.ID, "worker")
	require.NoError(t, err)
	_, labeled, err = store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{Label: "needs-review", EventType: "issue.labeled", Actor: "worker"})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(labeled.Payload), &payload))
	require.ElementsMatch(t, []any{firstClose, events[0].UID}, payload["after_close_uids"])
	_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	history, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)
	var snapshot db.IssueDocumentPayload
	for _, event := range history {
		if event.Type == "issue.snapshot" {
			require.NoError(t, json.Unmarshal([]byte(event.Payload), &snapshot))
		}
	}
	require.NotNil(t, snapshot.NeedsReviewContext)
	require.ElementsMatch(t, []string{firstClose, events[0].UID}, snapshot.NeedsReviewContext.Closes)
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
	// Advance only the hub's clock before it adds the label.
	clockEvent := remoteEventFromStored(created)
	clockEvent.EventUID = "00000000000000000000000001"
	clockEvent.Type = "issue.updated"
	clockEvent.Payload = []byte(`{}`)
	clockEvent.HLCPhysicalMS += 60000
	clockEvent.ContentHash = remoteEventHash(t, clockEvent)
	_, err = hub.InsertRemoteEvent(ctx, hubProject.ID, clockEvent)
	require.NoError(t, err)
	hubIssue, err := hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, labeled, err := hub.AddLabelAndEvent(ctx, hubIssue.ID, db.LabelEventParams{Label: "needs-review", EventType: "issue.labeled", Actor: "reviewer"})
	require.NoError(t, err)
	_, events, changed, err := spoke.CloseIssueWithEvents(ctx, issue.ID, "done", "worker", "", nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, events, 1)
	require.Greater(t, labeled.HLCPhysicalMS, events[0].HLCPhysicalMS)
	_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hubProject.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker",
		Events: []db.FederationIngestEvent{{SourceEventID: events[0].ID, Event: remoteEventFromStored(events[0])}},
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
	_, relabeled, err := hub.AddLabelAndEvent(ctx, hubIssue.ID, db.LabelEventParams{Label: "needs-review", EventType: "issue.labeled", Actor: "reviewer"})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(relabeled.Payload), &payload))
	require.Equal(t, []any{events[0].UID}, payload["after_close_uids"])
	_, err = spoke.InsertRemoteEvent(ctx, project.ID, remoteEventFromStored(relabeled))
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, project.ID))
	present, err = spoke.HasLabel(ctx, issue.ID, "needs-review")
	require.NoError(t, err)
	require.True(t, present)

	// An older peer sends a label without close context on a clock ahead of the close.
	legacy := remoteEventFromStored(relabeled)
	legacy.EventUID = "00000000000000000000000002"
	legacy.HLCPhysicalMS += 60000
	legacy.Payload = []byte(`{"label":"needs-review"}`)
	legacy.ContentHash = remoteEventHash(t, legacy)
	_, err = spoke.InsertRemoteEvent(ctx, project.ID, legacy)
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, project.ID))
	assertLegacyFold := func(store db.Storage, projectID, issueID int64) {
		t.Helper()
		history, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: projectID, IssueUID: issue.UID, Limit: 100})
		require.NoError(t, err)
		var last db.Event
		for _, event := range history {
			if event.Type != "issue.labeled" && event.Type != "issue.unlabeled" {
				continue
			}
			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
			if payload["label"] != "needs-review" {
				continue
			}
			if event.HLCPhysicalMS > last.HLCPhysicalMS || (event.HLCPhysicalMS == last.HLCPhysicalMS && event.HLCCounter > last.HLCCounter) {
				last = event
			}
		}
		require.Equal(t, "issue.unlabeled", last.Type)
		present, err := store.HasLabel(ctx, issueID, "needs-review")
		require.NoError(t, err)
		require.False(t, present)
	}
	assertLegacyFold(spoke, project.ID, issue.ID)
	legacy.EventUID = "00000000000000000000000003"
	legacy.OriginInstanceUID = spoke.InstanceUID()
	legacy.Actor = "worker"
	legacy.ContentHash = remoteEventHash(t, legacy)
	_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hubProject.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker",
		Events: []db.FederationIngestEvent{{SourceEventID: 999, Event: legacy}},
	})
	require.NoError(t, err)
	assertLegacyFold(hub, hubProject.ID, hubIssue.ID)
	_, _, _, err = spoke.ReopenIssue(ctx, issue.ID, "worker")
	require.NoError(t, err)
	assertLegacyFold(spoke, project.ID, issue.ID)
	return nil
}
