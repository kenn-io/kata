package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunArchivedFederationPinExport ensures the live-only export filter still
// includes public authority pins needed to restore retained federation bindings.
func RunArchivedFederationPinExport(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: project.UID, Actor: "example-actor", Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)
	publicKey, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: "00000000000000000000000002",
		KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
	}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "operator"})
	require.NoError(t, err)

	attribution, ok := store.(db.AttributionStorage)
	require.True(t, ok)
	found := false
	for record, err := range attribution.ExportAttribution(ctx, db.ExportFilter{IncludeDeleted: false}) {
		require.NoError(t, err)
		if exportedPin, ok := record.(*db.RootKeyPin); ok && exportedPin.ProjectUID == project.UID {
			found = true
			require.Equal(t, pin.AuthorityUID, exportedPin.AuthorityUID)
		}
	}
	assert.True(t, found, "a retained federation binding must keep its public root pin in filtered exports")
}

// RunEventReferenceProjectScope verifies that event feeds replace references
// to inaccessible issues with identity-free reset markers. Digest windows and
// selected-issue history continue to omit events that cannot be projected.
func RunEventReferenceProjectScope(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := context.Background()
	visibleProject, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	hiddenProject, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)

	visiblePeer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Visible peer", Author: "member",
	})
	require.NoError(t, err)
	hiddenPeer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hiddenProject.ID, Title: "Restricted peer", Author: "member",
	})
	require.NoError(t, err)

	createdWithHiddenLink, hiddenCreateEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Linked task", Author: "member",
		Links: []db.InitialLink{{
			Type: "related", ToNumber: hiddenPeer.ID, ExpectedProjectUID: hiddenProject.UID,
		}},
	})
	require.NoError(t, err)
	assert.Contains(t, hiddenCreateEvent.Payload, hiddenPeer.UID,
		"the issue.created fixture must carry the endpoint UID")
	assert.Contains(t, hiddenCreateEvent.Payload, hiddenPeer.ShortID,
		"the issue.created fixture must carry the endpoint short ID")

	createdWithVisibleLink, visibleCreateEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Visible linked task", Author: "member",
		Links: []db.InitialLink{{
			Type: "related", ToNumber: visiblePeer.ID, ExpectedProjectUID: visibleProject.UID,
		}},
	})
	require.NoError(t, err)

	hiddenSnapshot := newRemoteEvent(t, visibleProject, &createdWithHiddenLink.UID,
		"issue.snapshot", "member", store.InstanceUID(), 700,
		jsontext.Value(`{"uid":"`+createdWithHiddenLink.UID+`","short_id":"`+
			createdWithHiddenLink.ShortID+`","title":"Linked task","links":[{"type":"related","to_issue_uid":"`+
			hiddenPeer.UID+`","to_short_id":"`+hiddenPeer.ShortID+`"}]}`))
	_, err = store.InsertRemoteEvent(ctx, visibleProject.ID, hiddenSnapshot)
	require.NoError(t, err)
	hiddenSnapshotRows, err := store.EventsByUIDs(ctx, visibleProject.ID, []string{hiddenSnapshot.EventUID})
	require.NoError(t, err)
	require.Len(t, hiddenSnapshotRows, 1)
	visibleSnapshot := newRemoteEvent(t, visibleProject, &createdWithVisibleLink.UID,
		"issue.snapshot", "member", store.InstanceUID(), 701,
		jsontext.Value(`{"uid":"`+createdWithVisibleLink.UID+`","short_id":"`+
			createdWithVisibleLink.ShortID+`","title":"Visible linked task","links":[{"type":"related","to_issue_uid":"`+
			visiblePeer.UID+`","to_short_id":"`+visiblePeer.ShortID+`"}]}`))
	_, err = store.InsertRemoteEvent(ctx, visibleProject.ID, visibleSnapshot)
	require.NoError(t, err)

	closedWithHiddenParent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Task with restricted parent", Author: "member",
	})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: closedWithHiddenParent.ID, ToIssueID: hiddenPeer.ID,
		Type: "parent", Author: "member",
	})
	require.NoError(t, err)
	_, hiddenCloseEvents, _, err := store.CloseIssueWithEvents(
		ctx, closedWithHiddenParent.ID, "done", "member", "Completed task.", nil,
	)
	require.NoError(t, err)
	require.Len(t, hiddenCloseEvents, 1)
	assert.Contains(t, hiddenCloseEvents[0].Payload, hiddenPeer.UID,
		"the issue.closed fixture must carry the close-time parent UID")
	assert.Contains(t, hiddenCloseEvents[0].Payload, hiddenPeer.ShortID,
		"the issue.closed fixture must carry the close-time parent short ID")

	closedWithVisibleParent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Task with visible parent", Author: "member",
	})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: closedWithVisibleParent.ID, ToIssueID: visiblePeer.ID,
		Type: "parent", Author: "member",
	})
	require.NoError(t, err)
	_, visibleCloseEvents, _, err := store.CloseIssueWithEvents(
		ctx, closedWithVisibleParent.ID, "done", "member", "Completed task.", nil,
	)
	require.NoError(t, err)
	require.Len(t, visibleCloseEvents, 1)
	closedWithHiddenUIDEvidence, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Close evidence with restricted UID", Author: "member",
	})
	require.NoError(t, err)
	_, hiddenUIDEvidenceEvents, _, err := store.CloseIssueWithEvents(ctx, closedWithHiddenUIDEvidence.ID,
		"done", "member", "Completed task.", []db.Evidence{{Type: "duplicate-of", IssueRef: hiddenPeer.UID}})
	require.NoError(t, err)
	require.Len(t, hiddenUIDEvidenceEvents, 1)
	closedWithHiddenShortIDEvidence, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Close evidence with restricted short ID", Author: "member",
	})
	require.NoError(t, err)
	_, hiddenShortIDEvidenceEvents, _, err := store.CloseIssueWithEvents(ctx, closedWithHiddenShortIDEvidence.ID,
		"done", "member", "Completed task.", []db.Evidence{{Type: "superseded-by", IssueRef: hiddenProject.Name + "#" + hiddenPeer.ShortID}})
	require.NoError(t, err)
	require.Len(t, hiddenShortIDEvidenceEvents, 1)
	closedWithVisibleEvidence, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Close evidence with visible short ID", Author: "member",
	})
	require.NoError(t, err)
	_, visibleEvidenceEvents, _, err := store.CloseIssueWithEvents(ctx, closedWithVisibleEvidence.ID,
		"done", "member", "Completed task.", []db.Evidence{{Type: "duplicate-of", IssueRef: visiblePeer.ShortID}})
	require.NoError(t, err)
	require.Len(t, visibleEvidenceEvents, 1)
	closedWithVisibleUIDEvidence, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Close evidence with visible UID", Author: "member",
	})
	require.NoError(t, err)
	_, visibleUIDEvidenceEvents, _, err := store.CloseIssueWithEvents(ctx, closedWithVisibleUIDEvidence.ID,
		"done", "member", "Completed task.", []db.Evidence{{Type: "superseded-by", IssueRef: visiblePeer.UID}})
	require.NoError(t, err)
	require.Len(t, visibleUIDEvidenceEvents, 1)
	hiddenReference := hiddenProject.Name + "#" + hiddenPeer.ShortID
	throttledEvent, err := store.InsertCloseThrottledEvent(ctx, closedWithVisibleParent.ID, "member", db.CloseThrottledPayload{
		Reason: db.CloseThrottleReasonSiblingBurst,
		Parent: hiddenReference,
		Cohort: []string{hiddenReference},
		Prior:  &hiddenReference,
	})
	require.NoError(t, err)

	scoped := db.WithExposedIssues(db.WithAuthorizedProjects(ctx, []string{visibleProject.UID}))
	hiddenUIDs := []string{
		hiddenCreateEvent.UID,
		hiddenSnapshot.EventUID,
		hiddenCloseEvents[0].UID,
		hiddenUIDEvidenceEvents[0].UID,
		hiddenShortIDEvidenceEvents[0].UID,
		throttledEvent.UID,
	}
	visibleUIDs := []string{
		visibleCreateEvent.UID,
		visibleSnapshot.EventUID,
		visibleCloseEvents[0].UID,
		visibleEvidenceEvents[0].UID,
		visibleUIDEvidenceEvents[0].UID,
	}

	after, err := store.EventsAfter(scoped, db.EventsAfterParams{AfterID: 0, Limit: 1000})
	require.NoError(t, err)
	assertVisibleEventUIDs(t, after, hiddenUIDs, visibleUIDs)
	assert.NotContains(t, db.ExposedIssues(scoped), hiddenPeer.ID,
		"resolving a hidden reference for a reset marker must not expose the hidden issue")
	resetEventIDs := map[int64]struct{}{
		hiddenCreateEvent.ID:              {},
		hiddenSnapshotRows[0].ID:          {},
		hiddenCloseEvents[0].ID:           {},
		hiddenUIDEvidenceEvents[0].ID:     {},
		hiddenShortIDEvidenceEvents[0].ID: {},
		throttledEvent.ID:                 {},
	}
	for _, event := range after {
		if _, expected := resetEventIDs[event.ID]; !expected {
			continue
		}
		assert.Equal(t, db.ProjectScopeResetEventType, event.Type)
		assert.Empty(t, event.UID)
		assert.Nil(t, event.IssueUID)
		assert.Nil(t, event.RelatedIssueUID)
		assert.Empty(t, event.Payload)
		delete(resetEventIDs, event.ID)
	}
	assert.Empty(t, resetEventIDs, "each visible mutation with a hidden reference needs a reset cursor")
	departureIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Moved away from scoped project", Author: "member",
	})
	require.NoError(t, err)
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: departureIssue.ID, FromProjectID: visibleProject.ID, ToProjectID: hiddenProject.ID,
		IfMatchRev: departureIssue.Revision, Actor: "member",
	})
	require.NoError(t, err)
	pageAfterDeparture, err := store.EventsAfter(scoped, db.EventsAfterParams{
		AfterID: moved.EventID, ProjectID: visibleProject.ID, Limit: 100,
	})
	require.NoError(t, err)
	for _, event := range pageAfterDeparture {
		assert.NotEqual(t, moved.EventID, event.ID,
			"a departure reset must not bypass the cursor that already includes it")
	}
	pageBeforeDeparture, err := store.EventsAfter(scoped, db.EventsAfterParams{
		AfterID: 0, ThroughID: moved.EventID - 1, ProjectID: visibleProject.ID, Limit: 1000,
	})
	require.NoError(t, err)
	for _, event := range pageBeforeDeparture {
		assert.NotEqual(t, moved.EventID, event.ID,
			"a departure reset must not bypass the upper event cursor")
	}
	for _, event := range after {
		assert.Equal(t, visibleProject.UID, event.ProjectUID,
			"cross-project feeds must retain their project boundary")
	}

	window, err := store.EventsInWindow(scoped, db.EventsInWindowParams{
		Since: "2020-01-01T00:00:00.000Z", Until: "2035-01-01T00:00:00.000Z",
	})
	require.NoError(t, err)
	assertVisibleEventUIDs(t, window, hiddenUIDs, visibleUIDs)
	assert.NotContains(t, db.ExposedIssues(scoped), hiddenPeer.ID,
		"omitting a hidden digest reference must not expose the hidden issue")

	ui, ok := store.(db.UIStore)
	require.True(t, ok, "native storage must implement coherent UI snapshots")
	for _, tc := range []struct {
		issue     db.Issue
		hiddenUID []string
		visible   []string
	}{
		{issue: createdWithHiddenLink, hiddenUID: []string{hiddenCreateEvent.UID, hiddenSnapshot.EventUID}},
		{issue: createdWithVisibleLink, visible: []string{visibleCreateEvent.UID, visibleSnapshot.EventUID}},
		{issue: closedWithHiddenParent, hiddenUID: []string{hiddenCloseEvents[0].UID}},
		{issue: closedWithHiddenUIDEvidence, hiddenUID: []string{hiddenUIDEvidenceEvents[0].UID}},
		{issue: closedWithHiddenShortIDEvidence, hiddenUID: []string{hiddenShortIDEvidenceEvents[0].UID}},
		{issue: closedWithVisibleEvidence, visible: []string{visibleEvidenceEvents[0].UID}},
		{issue: closedWithVisibleUIDEvidence, visible: []string{visibleUIDEvidenceEvents[0].UID}},
		{issue: closedWithVisibleParent, hiddenUID: []string{throttledEvent.UID}, visible: []string{visibleCloseEvents[0].UID}},
	} {
		snapshot, err := ui.ReadUISnapshot(scoped, db.UISnapshotQuery{
			View: "all-open", SelectedIssueUID: tc.issue.UID, IncludeHistory: true,
		})
		require.NoError(t, err)
		assertVisibleEventUIDs(t, snapshot.History, tc.hiddenUID, tc.visible)
	}
}

func assertVisibleEventUIDs(t *testing.T, events []db.Event, hiddenUIDs, visibleUIDs []string) {
	t.Helper()
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		seen[event.UID] = struct{}{}
	}
	for _, uid := range hiddenUIDs {
		assert.NotContains(t, seen, uid,
			"event with a reference outside the caller's authorized projects must be hidden")
	}
	for _, uid := range visibleUIDs {
		assert.Contains(t, seen, uid,
			"event with only authorized references must remain visible")
	}
}

// RunEventReleaseExposure verifies that event reads register every issue a
// returned event names: its subject, its related issue, and its payload
// references. The project access release rechecks those issues, so a page read
// before one moved into a hidden project is withheld. A departure reset names
// no issue and registers nothing.
func RunEventReleaseExposure(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := context.Background()
	visibleProject, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	hiddenProject, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	subject, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Released subject", Author: "member",
	})
	require.NoError(t, err)
	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Released peer", Author: "member",
	})
	require.NoError(t, err)
	_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: subject.ID, ToIssueID: peer.ID, Type: "blocks", Author: "member",
	}, db.LinkEventParams{
		EventType: "issue.linked", EventIssueID: subject.ID, FromShortID: subject.ShortID, FromUID: subject.UID,
		ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "member",
	})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssueWithEvents(ctx, subject.ID, "done", "member", "Completed task.", nil)
	require.NoError(t, err)
	relayed, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Relayed subject", Author: "member",
	})
	require.NoError(t, err)
	snapshot := newRemoteEvent(t, visibleProject, &relayed.UID, "issue.snapshot", "member",
		store.InstanceUID(), 801, jsontext.Value(`{"uid":"`+relayed.UID+`","short_id":"`+
			relayed.ShortID+`","title":"Relayed subject"}`))
	_, err = store.InsertRemoteEvent(ctx, visibleProject.ID, snapshot)
	require.NoError(t, err)
	departed, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visibleProject.ID, Title: "Departed issue", Author: "member",
	})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: departed.ID, FromProjectID: visibleProject.ID, ToProjectID: hiddenProject.ID,
		IfMatchRev: departed.Revision, Actor: "member",
	})
	require.NoError(t, err)

	scoped := func() context.Context {
		return db.WithExposedIssues(db.WithAuthorizedProjects(ctx, []string{visibleProject.UID}))
	}
	after := scoped()
	_, err = store.EventsAfter(after, db.EventsAfterParams{AfterID: 0, Limit: 1000})
	require.NoError(t, err)
	assert.ElementsMatch(t, []int64{subject.ID, peer.ID, relayed.ID}, db.ExposedIssues(after),
		"an event page registers the issues it names and nothing moved out of reach")

	window := scoped()
	_, err = store.EventsInWindow(window, db.EventsInWindowParams{
		Since: "2020-01-01T00:00:00.000Z", Until: "2035-01-01T00:00:00.000Z",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []int64{subject.ID, peer.ID, relayed.ID}, db.ExposedIssues(window),
		"a digest window registers the issues it names and nothing moved out of reach")

	ui, ok := store.(db.UIStore)
	require.True(t, ok, "native storage must implement coherent UI snapshots")
	history := scoped()
	_, err = ui.ReadUISnapshot(history, db.UISnapshotQuery{
		View: "all-open", SelectedIssueUID: peer.UID, IncludeHistory: true,
	})
	require.NoError(t, err)
	assert.Contains(t, db.ExposedIssues(history), subject.ID,
		"the selected issue's history registers the closed subject of the link event naming it")
}

// RunUIDOnlyEventSubjectOutOfReach verifies that an event naming its subject
// by UID alone fails closed when the caller's boundary can no longer resolve
// that subject. The event query admitted it, so the subject left the caller's
// projects after the query: releasing the event would expose an issue the
// release check never registered. A move recorded by UID passes the query
// through its visible departure project after the issue moved on into a
// hidden project, which reaches the same state deterministically.
func RunUIDOnlyEventSubjectOutOfReach(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	require.NotNil(t, backend.DetachEventIssueID, "backend must detach event issue IDs")
	ctx := context.Background()
	source, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "example-workspace")
	require.NoError(t, err)
	hidden, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: source.ID, Title: "Relayed subject", Author: "member",
	})
	require.NoError(t, err)
	visibleMove, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID,
		IfMatchRev: issue.Revision, Actor: "member",
	})
	require.NoError(t, err)
	require.NoError(t, backend.DetachEventIssueID(ctx, store, visibleMove.EventID))
	moved, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: target.ID, ToProjectID: hidden.ID,
		IfMatchRev: moved.Revision, Actor: "member",
	})
	require.NoError(t, err)

	scoped := db.WithExposedIssues(db.WithAuthorizedProjects(ctx, []string{source.UID, target.UID}))
	page, err := store.EventsAfter(scoped, db.EventsAfterParams{
		AfterID: visibleMove.EventID - 1, ThroughID: visibleMove.EventID, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, page, 1, "the departure project admits the UID-only move")
	assert.Equal(t, db.ProjectScopeResetEventType, page[0].Type,
		"an unresolvable UID-only subject must become a reset marker")
	assert.Nil(t, page[0].IssueUID)
	assert.Empty(t, page[0].Payload)
	assert.NotContains(t, db.ExposedIssues(scoped), issue.ID)

	stream := db.WithAuthorizedProjects(ctx, []string{source.UID, target.UID})
	streamPage, err := store.EventsAfter(stream, db.EventsAfterParams{
		AfterID: visibleMove.EventID - 1, ThroughID: visibleMove.EventID, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, streamPage, 1)
	assert.Equal(t, db.ProjectScopeResetEventType, streamPage[0].Type,
		"a stream read, which keeps no exposure registry, fails closed the same way")
}
