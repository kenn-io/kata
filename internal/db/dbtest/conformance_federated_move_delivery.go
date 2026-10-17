package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
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
			editPayload, err := json.Marshal(map[string]string{"uid": issue.UID, "title": "after concurrent branch"})
			require.NoError(t, err)
			edit := newRemoteEvent(t, projects[2], &issue.UID, "issue.updated", "worker", origin, physical+2, editPayload)
			batch := []db.FederationIngestEvent{{SourceEventID: 11, Event: second}}
			if kind == "later branch wins" {
				batch = append(batch, db.FederationIngestEvent{SourceEventID: 12, Event: edit})
			}
			_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: projects[2].ID, SpokeInstanceUID: origin, BoundActor: "worker", FederationEnrollmentID: grants[2].Enrollment.ID, MoveSourceEnrollmentIDs: proofs, EventFeatures: db.SupportedEventFeatures, Events: batch})
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
			stored, err := store.EventsByUIDs(ctx, projects[2].ID, []string{second.EventUID})
			require.NoError(t, err)
			require.Len(t, stored, 1)
			require.Equal(t, second.ContentHash, stored[0].ContentHash, "departure notification cannot rewrite the move envelope")
			reset, err := store.PurgeResetCheck(ctx, before, projects[1].ID)
			require.NoError(t, err)
			page, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: projects[1].ID, AfterID: before, Limit: 10, Features: db.SupportedEventFeatures})
			require.NoError(t, err)
			if kind == "later branch wins" {
				require.Equal(t, stored[0].ID, reset, "actual displaced project resets at the winning move, not the later edit")
				require.Positive(t, page.ResetAfterID)
				require.Equal(t, stored[0].ID, page.ResetAfterID)
			} else {
				require.Zero(t, reset, "a losing branch does not displace the current project")
				require.Zero(t, page.ResetAfterID)
			}
			reset, err = store.PurgeResetCheck(ctx, stored[0].ID, projects[1].ID)
			require.NoError(t, err)
			require.Zero(t, reset, "clients past the departure cursor need no repeated reset")
		})
	}
	for _, kind := range []string{"pull", "reattach", "ordinary replay"} {
		t.Run("materialized displacement "+kind, func(t *testing.T) {
			local := make([]db.Project, 3)
			for i, name := range []string{"source", "middle", "target"} {
				var err error
				local[i], err = store.CreateProject(ctx, "displaced-"+strings.ReplaceAll(kind, " ", "-")+"-"+name)
				require.NoError(t, err)
				if kind != "reattach" || i != 2 {
					_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local[i].ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: int64(i + 1), HubProjectUID: local[i].UID, Enabled: true, PushEnabled: true, Actor: "worker"})
					require.NoError(t, err)
				}
			}
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: local[0].ID, Title: "retained move displacement", Author: "worker"})
			require.NoError(t, err)
			makeMove := func(to int, at int64) db.RemoteEvent {
				payload, err := json.Marshal(map[string]any{"issue_uid": issue.UID, "from_project_uid": local[0].UID, "from_short_id": issue.ShortID, "to_project_uid": local[to].UID, "to_short_id": issue.ShortID, "snapshot": map[string]string{"uid": issue.UID, "short_id": issue.ShortID, "title": issue.Title, "author": "worker", "status": "open"}})
				require.NoError(t, err)
				return newRemoteEvent(t, local[to], &issue.UID, "issue.moved", "worker", origin, at, payload)
			}
			at := time.Now().Add(time.Second).UnixMilli()
			_, err = store.InsertRemoteEvents(ctx, local[1].ID, []db.RemoteEvent{makeMove(1, at)})
			require.NoError(t, err)
			require.NoError(t, store.MaterializeFederatedProject(ctx, local[1].ID))
			departing, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			winner := makeMove(2, at+1)
			_, err = store.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{winner})
			require.NoError(t, err)
			if kind == "reattach" {
				require.NoError(t, store.MaterializeFederatedProject(ctx, local[0].ID))
				_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local[2].ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 3, HubProjectUID: local[2].UID, Enabled: true, PushEnabled: true, Actor: "worker"})
				require.NoError(t, err)
			}
			if kind == "ordinary replay" {
				payload, err := json.Marshal(map[string]string{"uid": issue.UID, "title": "later ordinary history"})
				require.NoError(t, err)
				_, err = store.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{newRemoteEvent(t, local[0], &issue.UID, "issue.updated", "worker", origin, at+2, payload)})
				require.NoError(t, err)
			}
			seen, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.NoError(t, store.MaterializeFederatedProject(ctx, local[2].ID))
			got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, local[2].ID, got.ProjectID)
			reset, err := store.PurgeResetCheck(ctx, seen, local[1].ID)
			require.NoError(t, err)
			require.Greater(t, reset, seen, "a reader can already be past the retained winning move before materialization")
			query := store.(interface {
				QueryRowContext(context.Context, string, ...any) *sql.Row
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			})
			var controlUID, actor, payload string
			var subject, related sql.NullString
			require.NoError(t, query.QueryRowContext(ctx, `SELECT uid,actor,payload,issue_uid,related_issue_uid FROM events WHERE id=$1`, reset).Scan(&controlUID, &actor, &payload, &subject, &related))
			require.Equal(t, "system", actor)
			expectedReference, err := json.Marshal(map[string]string{"from_project_uid": local[1].UID, "from_short_id": departing.ShortID, "move_event_uid": winner.EventUID})
			require.NoError(t, err)
			require.JSONEq(t, string(expectedReference), payload, "local control retains the actual displaced reference")
			require.True(t, subject.Valid)
			require.Equal(t, issue.UID, subject.String, "local control follows subject history cleanup")
			require.False(t, related.Valid)
			var forwarding int
			require.NoError(t, query.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM federation_event_provenance WHERE event_uid=$1)+(SELECT COUNT(*) FROM federation_relay_outbox WHERE source_uid=$1)`, controlUID).Scan(&forwarding))
			require.Zero(t, forwarding, "local reset control has no root receipt or relay output")
			pending, err := store.PendingFederationPushEvents(ctx, local[1].ID, store.InstanceUID(), seen, 10)
			require.NoError(t, err)
			require.Empty(t, pending, "local reset is not native spoke intent")
			// A portable history without its optional reset metadata still
			// cannot export local control rows as federated source events.
			_, err = query.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, db.FederationPurgeResetMetadataPrefix+local[1].UID)
			require.NoError(t, err)
			page, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: local[1].ID, AfterID: seen, Limit: 10, Features: db.SupportedEventFeatures})
			require.NoError(t, err)
			require.Empty(t, page.Events, "native read never exports local reset controls")
			beforeRepeat, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.NoError(t, store.MaterializeFederatedProject(ctx, local[2].ID))
			afterRepeat, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, beforeRepeat, afterRepeat, "unchanged replay cannot allocate repeated resets")
		})
	}
	return nil
}
