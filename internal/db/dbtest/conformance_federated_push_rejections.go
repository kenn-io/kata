package dbtest

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedMissingEnrollmentPartialBatch(t *testing.T, store db.Storage) error {
	for _, kind := range []string{"mixed", "all-rejected", "payload-only", "moved-peer", "rejected-create", "dependent-on-rejected-create", "rejected-move"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			source, target, moved, _ := federatedRehomeFixture(t, store, "partial-"+kind)
			_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: moved.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: moved.Revision, Actor: "worker"})
			require.NoError(t, err)
			good, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Independent", Author: "worker"})
			require.NoError(t, err)
			origin, err := uid.New()
			require.NoError(t, err)
			grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "worker"})
			require.NoError(t, err)
			at := time.Now().Add(time.Hour).UnixMilli()
			bad := newRemoteEvent(t, source, &moved.UID, "issue.updated", "worker", origin, at, []byte(`{"uid":"`+moved.UID+`","title":"Forbidden"}`))
			if kind == "payload-only" {
				bad.IssueUID = nil
			}
			if kind == "moved-peer" {
				bad = newRemoteEvent(t, source, &good.UID, "issue.linked", "worker", origin, at, []byte(`{"from_uid":"`+good.UID+`","to_uid":"`+moved.UID+`","type":"related"}`))
				bad.RelatedIssueUID = &moved.UID
			}
			freshUID, err := uid.New()
			require.NoError(t, err)
			if kind == "rejected-create" || kind == "dependent-on-rejected-create" {
				payload, err := json.Marshal(map[string]any{"uid": freshUID, "title": "Rejected creation", "author": "worker", "status": "open", "links": []map[string]string{{"type": "related", "to_issue_uid": moved.UID}}})
				require.NoError(t, err)
				bad = newRemoteEvent(t, source, &freshUID, "issue.created", "worker", origin, at, payload)
			}
			if kind == "rejected-move" {
				payload, err := json.Marshal(map[string]any{"issue_uid": moved.UID, "from_project_uid": target.UID, "from_short_id": moved.ShortID, "to_project_uid": source.UID, "to_short_id": moved.ShortID, "snapshot": map[string]string{"uid": moved.UID, "title": moved.Title, "author": "worker", "short_id": moved.ShortID, "status": "open"}})
				require.NoError(t, err)
				bad = newRemoteEvent(t, source, &moved.UID, "issue.moved", "worker", origin, at, payload)
			}
			bad.ContentHash, err = db.RemoteEventContentHash(bad)
			require.NoError(t, err)
			valid := newRemoteEvent(t, source, &good.UID, "issue.updated", "worker", origin, at, []byte(`{"uid":"`+good.UID+`","title":"Arrived"}`))
			params := db.FederationIngestParams{AllowEventRejections: true, ProjectID: source.ID, FederationEnrollmentID: grant.Enrollment.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: bad}, {SourceEventID: 2, Event: valid}}}
			if kind == "all-rejected" {
				params.Events = params.Events[:1]
			}
			if kind == "dependent-on-rejected-create" {
				dependent := newRemoteEvent(t, source, &freshUID, "issue.updated", "worker", origin, at, []byte(`{"uid":"`+freshUID+`","title":"Must not be known"}`))
				params.Events = append(params.Events, db.FederationIngestEvent{SourceEventID: 3, Event: dependent})
			}
			if kind == "rejected-move" {
				afterMove := newRemoteEvent(t, source, &moved.UID, "issue.updated", "worker", origin, at, []byte(`{"uid":"`+moved.UID+`","title":"Must not acquire route authority"}`))
				params.Events = append(params.Events, db.FederationIngestEvent{SourceEventID: 3, Event: afterMove})
			}
			before, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			if kind == "mixed" {
				params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: 0}
				_, err := store.IngestFederationEvents(ctx, params)
				require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized, "supplied invalid proof remains an atomic credential failure")
				params.MoveSourceEnrollmentIDs = nil
			}
			result, err := store.IngestFederationEvents(ctx, params)
			if kind == "dependent-on-rejected-create" {
				require.Error(t, err, "a rejected creation cannot prove a later primary issue")
				after, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				require.Equal(t, before, after, "ordinary dependent validation still rolls back atomically")
				return
			}
			require.NoError(t, err, "only missing current-project enrollment is an event rejection")
			if kind == "rejected-move" {
				require.Len(t, result.RejectedEvents, 2)
			} else {
				require.Len(t, result.RejectedEvents, 1)
			}
			require.Equal(t, bad.EventUID, result.RejectedEvents[0].EventUID)
			require.Equal(t, moved.UID, result.RejectedEvents[0].IssueUID)
			require.Equal(t, target.UID, result.RejectedEvents[0].CurrentProjectUID)
			require.Equal(t, target.Name, result.RejectedEvents[0].CurrentProjectName)
			got, err := store.IssueByID(ctx, good.ID)
			require.NoError(t, err)
			if kind == "all-rejected" {
				require.Zero(t, result.Accepted)
				require.Empty(t, result.InsertedEventUIDs)
				require.Equal(t, int64(1), result.PushCursorEventID)
				require.Equal(t, "Independent", got.Title)
			} else {
				require.Equal(t, 1, result.Accepted)
				require.Equal(t, []string{valid.EventUID}, result.InsertedEventUIDs)
				if kind == "rejected-move" {
					require.Equal(t, int64(3), result.PushCursorEventID)
				} else {
					require.Equal(t, int64(2), result.PushCursorEventID)
				}
				require.Equal(t, "Arrived", got.Title)
			}
			got, err = store.IssueByID(ctx, moved.ID)
			require.NoError(t, err)
			require.Equal(t, moved.Title, got.Title)
			if kind == "mixed" {
				current, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &target.ID, Capabilities: "push,pull", Actor: "worker"})
				require.NoError(t, err)
				params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: current.Enrollment.ID}
				accepted, err := store.IngestFederationEvents(ctx, params)
				require.NoError(t, err)
				require.Equal(t, 1, accepted.Accepted)
				require.Equal(t, 1, accepted.Duplicates)
				require.NoError(t, store.RevokeFederationEnrollment(ctx, current.Enrollment.ID))
				params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: 0}
				duplicate, err := store.IngestFederationEvents(ctx, params)
				require.NoError(t, err, "accepted history acknowledges after supplemental proof disappears")
				require.Equal(t, 2, duplicate.Duplicates)
				require.Empty(t, duplicate.RejectedEvents)
			}
		})
	}
	return nil
}
