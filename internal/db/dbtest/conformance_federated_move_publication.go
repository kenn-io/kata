package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedMovedIssueCurrentProjectReset(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "late-publication-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "late-publication-target")
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "original title", Author: "worker"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	cold := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, cold.Close()) })
	locals := map[string]db.Project{}
	for _, project := range []db.Project{source, target} {
		local, err := cold.CreateProjectWithUID(ctx, project.Name, project.UID)
		require.NoError(t, err)
		_, err = cold.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		locals[project.UID] = local
	}
	initial, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: target.ID, Limit: 100, Features: db.SupportedEventFeatures})
	require.NoError(t, err)
	var history []db.RemoteEvent
	for _, event := range initial.Events {
		history = append(history, remoteMoveEvent(event))
	}
	_, err = cold.InsertRemoteEvents(ctx, locals[target.UID].ID, history)
	require.NoError(t, err)
	require.NoError(t, cold.MaterializeFederatedProject(ctx, locals[target.UID].ID))
	origin, err := uid.New()
	require.NoError(t, err)
	grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, Capabilities: "push,pull", Actor: "worker"})
	require.NoError(t, err)
	event := newRemoteEvent(t, source, &issue.UID, "issue.updated", "worker", origin, time.Now().Add(time.Hour).UnixMilli(), jsontext.Value(`{"title":"delayed source title"}`))
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	params := db.FederationIngestParams{ProjectID: source.ID, SpokeInstanceUID: origin, BoundActor: "worker", FederationEnrollmentID: grant.Enrollment.ID, EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}}
	result, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
	require.Equal(t, "delayed source title", got.Title)
	retained, err := store.EventsByUIDs(ctx, source.ID, []string{event.EventUID})
	require.NoError(t, err)
	require.Len(t, retained, 1)
	require.Equal(t, source.UID, retained[0].ProjectUID)
	require.Equal(t, event.ContentHash, retained[0].ContentHash)
	reset, err := store.PurgeResetCheck(ctx, before, target.ID)
	require.NoError(t, err)
	require.Equal(t, retained[0].ID, reset, "current destination must invalidate at the delayed event cursor")
	require.Contains(t, result.Resets, db.FederationProjectReset{ProjectID: target.ID, EventID: reset})
	read, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: target.ID, AfterID: before, Limit: 100, Features: db.SupportedEventFeatures})
	require.NoError(t, err)
	require.Zero(t, read.ResetAfterID, "local view invalidation must not discard portable move history")
	var foundEnvelope bool
	for _, projected := range read.Events {
		if projected.UID == event.EventUID {
			foundEnvelope = true
			require.Equal(t, source.UID, projected.ProjectUID)
			require.Equal(t, event.ContentHash, projected.ContentHash)
		}
	}
	require.True(t, foundEnvelope, "native destination feed already includes the historical source envelope")
	maxID, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	duplicate, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	require.Empty(t, duplicate.Resets)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, maxID, after)
	_, err = cold.InsertRemoteEvents(ctx, locals[source.UID].ID, []db.RemoteEvent{remoteMoveEvent(retained[0])})
	require.NoError(t, err)
	seen, err := cold.MaxEventID(ctx)
	require.NoError(t, err)
	applyCtx, capture := db.WithFederationMoveResetCapture(ctx)
	applyCtx = db.WithFederationPulledEventUIDs(applyCtx, []string{event.EventUID})
	require.NoError(t, cold.MaterializeFederatedProject(applyCtx, locals[source.UID].ID))
	applied, err := cold.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "delayed source title", applied.Title)
	pullReset, err := cold.PurgeResetCheck(ctx, seen, locals[target.UID].ID)
	require.NoError(t, err)
	require.Greater(t, pullReset, seen, "pull apply must allocate a cursor after its already-retained source event")
	require.Contains(t, capture.Resets, db.FederationProjectReset{ProjectID: locals[target.UID].ID, EventID: pullReset})
	require.NoError(t, cold.MaterializeFederatedProject(ctx, locals[source.UID].ID))
	noOp, err := cold.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, pullReset, noOp, "ordinary replay must not create repeated current-owner controls")
	t.Run("timed assignment", func(t *testing.T) {
		source, err := store.CreateProject(ctx, "late-renewal-source")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, "late-renewal-target")
		require.NoError(t, err)
		for _, project := range []db.Project{source, target} {
			_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
			require.NoError(t, err)
		}
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "timed assignment", Author: "worker"})
		require.NoError(t, err)
		claimed, err := store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: "worker", TTL: 2 * time.Hour})
		require.NoError(t, err)
		if claimed.Event == nil || claimed.Issue.AssignmentExpiresOn == nil {
			t.Fatal("missing timed assignment")
		}
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: claimed.Issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		oldExpiry, _ := db.StringValue(db.PayloadMap([]byte(claimed.Event.Payload))["assignment_expires_on"])
		renewed := claimed.Issue.AssignmentExpiresOn.Add(time.Hour)
		payload, err := json.Marshal(map[string]string{"owner": "worker", "old_assignment_expires_on": oldExpiry, "assignment_expires_on": renewed.UTC().Format(time.RFC3339Nano)})
		require.NoError(t, err)
		event := newRemoteEvent(t, source, &issue.UID, "issue.assignment_renewed", "worker", origin, time.Now().Add(time.Hour).UnixMilli(), payload)
		before, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		result, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: source.ID, SpokeInstanceUID: origin, BoundActor: "worker", FederationEnrollmentID: grant.Enrollment.ID, EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 2, Event: event}}})
		require.NoError(t, err)
		got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, &renewed, got.AssignmentExpiresOn)
		reset, err := store.PurgeResetCheck(ctx, before, target.ID)
		require.NoError(t, err)
		require.Greater(t, reset, before, "conditional owner folds still require current-project invalidation")
		require.Contains(t, result.Resets, db.FederationProjectReset{ProjectID: target.ID, EventID: reset})
	})
	return nil
}
