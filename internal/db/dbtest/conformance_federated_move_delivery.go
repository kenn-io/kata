package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedMoveDelivery(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	projects := make([]db.Project, 3)
	for i, name := range []string{"delivery-source", "delivery-middle", "delivery-target"} {
		var err error
		projects[i], err = store.CreateProject(ctx, name)
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, projects[i].ID, "worker")
		require.NoError(t, err)
	}
	origin, err := uid.New()
	require.NoError(t, err)
	moveEvent := func(issue db.Issue, from, to int, physical int64) db.RemoteEvent {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"issue_uid": issue.UID, "from_project_uid": projects[from].UID, "from_short_id": issue.ShortID, "to_project_uid": projects[to].UID, "to_short_id": issue.ShortID, "updated_at": time.UnixMilli(physical).UTC().Format(time.RFC3339Nano), "snapshot": map[string]string{"uid": issue.UID, "short_id": issue.ShortID, "title": issue.Title, "author": "worker", "status": "open"}})
		require.NoError(t, err)
		return newRemoteEvent(t, projects[to], &issue.UID, "issue.moved", "worker", origin, physical, jsontext.Value(payload))
	}
	t.Run("fresh push requires move snapshot", func(t *testing.T) {
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "fresh snapshot required", Author: "worker"})
		require.NoError(t, err)
		event := moveEvent(issue, 0, 1, time.Now().UnixMilli())
		payload := db.PayloadMap(event.Payload)
		delete(payload, "snapshot")
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		event = newRemoteEvent(t, projects[1], &issue.UID, "issue.moved", "worker", origin, event.HLCPhysicalMS, raw)
		before, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: projects[1].ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
		after, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, before, after)
		got, err := store.IssueByID(ctx, issue.ID)
		require.NoError(t, err)
		require.Equal(t, projects[0].ID, got.ProjectID)
	})
	t.Run("pre-federation history starts at covering baseline", func(t *testing.T) {
		local, err := store.CreateProject(ctx, "history-local-source")
		require.NoError(t, err)
		baseline, err := store.CreateProject(ctx, "history-baseline-source")
		require.NoError(t, err)
		cursor, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: local.ID, Title: "local move before federation", Author: "worker"})
		require.NoError(t, err)
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: local.ID, ToProjectID: baseline.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, baseline.ID, "worker")
		require.NoError(t, err)
		issue, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: baseline.ID, ToProjectID: projects[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		for _, subject := range []string{"", issue.UID} {
			page, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: projects[2].ID, IssueUID: subject, AfterID: cursor, Limit: 1, Features: db.SupportedEventFeatures})
			require.NoError(t, err)
			require.Len(t, page.Events, 1)
			require.Equal(t, "issue.snapshot", page.Events[0].Type, "baseline exclusion must happen before pagination")
			require.Equal(t, baseline.UID, page.Events[0].ProjectUID)
			require.Greater(t, page.NextAfterID, cursor)
		}
	})
	t.Run("creation earlier in return move batch", func(t *testing.T) {
		issueUID, err := uid.New()
		require.NoError(t, err)
		issueShortID, err := shortid.Derive(issueUID, shortid.MinLength)
		require.NoError(t, err)
		issue := db.Issue{UID: issueUID, ShortID: issueShortID, Title: "round trip"}
		physical := time.Now().UnixMilli()
		payload, err := json.Marshal(map[string]string{"uid": issueUID, "short_id": issue.ShortID, "title": issue.Title, "author": "worker", "status": "open"})
		require.NoError(t, err)
		creation := newRemoteEvent(t, projects[0], &issueUID, "issue.created", "worker", origin, physical, jsontext.Value(payload))
		returned := moveEvent(issue, 1, 0, physical+2)
		result, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: projects[0].ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: creation}, {SourceEventID: 3, Event: returned}}})
		require.NoError(t, err)
		require.Equal(t, 2, result.Accepted)
		got, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, projects[0].ID, got.ProjectID)
	})
	t.Run("predecessor delivery retries", func(t *testing.T) {
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "reverse delivery", Author: "worker"})
		require.NoError(t, err)
		physical := time.Now().Add(time.Second).UnixMilli()
		first, last := moveEvent(issue, 0, 1, physical), moveEvent(issue, 1, 2, physical+1)
		lastParams := db.FederationIngestParams{ProjectID: projects[2].ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 5, Event: last}}}
		_, err = store.IngestFederationEvents(ctx, lastParams)
		require.ErrorIs(t, err, db.ErrNotFound, "the predecessor is not an invalid move")
		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: projects[1].ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 4, Event: first}}})
		require.NoError(t, err)
		_, err = store.IngestFederationEvents(ctx, lastParams)
		require.NoError(t, err)
		got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, projects[2].ID, got.ProjectID)
	})
	for _, kind := range []string{"later branch wins", "earlier branch loses", "current owner proof required"} {
		t.Run(kind, func(t *testing.T) {
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "concurrent branches", Author: "worker"})
			require.NoError(t, err)
			physical := time.Now().Add(time.Second).UnixMilli()
			firstClock, secondClock := physical, physical+1
			if kind == "earlier branch loses" {
				firstClock, secondClock = secondClock, firstClock
			}
			grants := make([]db.CreatedFederationEnrollment, len(projects))
			for i, project := range projects {
				grants[i], err = store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &project.ID, Capabilities: "push,pull", Actor: "worker"})
				require.NoError(t, err)
			}
			first := moveEvent(issue, 0, 1, firstClock)
			_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: projects[1].ID, SpokeInstanceUID: origin, BoundActor: "worker", FederationEnrollmentID: grants[1].Enrollment.ID, MoveSourceEnrollmentIDs: map[string]int64{projects[0].UID: grants[0].Enrollment.ID}, EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 10, Event: first}}})
			require.NoError(t, err)
			second := moveEvent(issue, 0, 2, secondClock)
			proofs := map[string]int64{projects[0].UID: grants[0].Enrollment.ID}
			if kind != "current owner proof required" {
				proofs[projects[1].UID] = grants[1].Enrollment.ID
			}
			before, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: projects[2].ID, SpokeInstanceUID: origin, BoundActor: "worker", FederationEnrollmentID: grants[2].Enrollment.ID, MoveSourceEnrollmentIDs: proofs, EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 11, Event: second}}})
			if kind == "current owner proof required" {
				require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
				after, readErr := store.MaxEventID(ctx)
				require.NoError(t, readErr)
				require.Equal(t, before, after, "missing current-owner authority must reject atomically")
				return
			}
			require.NoError(t, err, "a known historical source is a concurrent branch, not an undelivered predecessor")
			got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			winner := projects[2].ID
			if kind == "earlier branch loses" {
				winner = projects[1].ID
			}
			require.Equal(t, winner, got.ProjectID, "retained deterministic clocks decide concurrent ownership")
		})
	}
	return nil
}
