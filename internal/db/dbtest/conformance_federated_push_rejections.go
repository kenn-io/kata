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
	for _, kind := range []string{"mixed", "revoked-race", "all-rejected", "payload-only", "moved-peer", "dependent-on-rejected-create", "rejected-move", "malformed-comment"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			rejectedMove := kind == "rejected-move"
			hasDependent := rejectedMove || kind == "dependent-on-rejected-create"
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
			if kind == "malformed-comment" {
				commentUID, err := uid.New()
				require.NoError(t, err)
				// Malformed actor attribution is atomic poison even when the same
				// event also lacks authority for its issue's current project.
				bad = newRemoteEvent(t, source, &moved.UID, "issue.commented", "worker", origin, at, []byte(`{"comment_uid":"`+commentUID+`","author":"worker","body":"Malformed","teammate":23}`))
			}
			if kind == "payload-only" {
				// Older federated history can carry issue identity only in the payload.
				bad.IssueUID = nil
			}
			if kind == "moved-peer" {
				bad = newRemoteEvent(t, source, &good.UID, "issue.linked", "worker", origin, at, []byte(`{"from_uid":"`+good.UID+`","to_uid":"`+moved.UID+`","type":"related"}`))
				bad.RelatedIssueUID = &moved.UID
			}
			freshUID, err := uid.New()
			require.NoError(t, err)
			if kind == "dependent-on-rejected-create" {
				payload, err := json.Marshal(map[string]any{"uid": freshUID, "title": "Rejected creation", "author": "worker", "status": "open", "links": []map[string]string{{"type": "related", "to_issue_uid": moved.UID}}})
				require.NoError(t, err)
				bad = newRemoteEvent(t, source, &freshUID, "issue.created", "worker", origin, at, payload)
			}
			if rejectedMove {
				// A spoke can retain a move authored while target enrollment was valid,
				// then lose that enrollment before delivering it through the source.
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
				params.Events = []db.FederationIngestEvent{{SourceEventID: 1, Event: bad}, {SourceEventID: 2, Event: dependent}, {SourceEventID: 3, Event: valid}}
			}
			if rejectedMove {
				afterMove := newRemoteEvent(t, source, &moved.UID, "issue.updated", "worker", origin, at, []byte(`{"uid":"`+moved.UID+`","title":"Must not acquire route authority"}`))
				params.Events = []db.FederationIngestEvent{{SourceEventID: 1, Event: bad}, {SourceEventID: 2, Event: afterMove}, {SourceEventID: 3, Event: valid}}
			}
			before, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			if kind == "mixed" {
				params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: 0}
			}
			if kind == "revoked-race" {
				current, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &target.ID, Capabilities: "push,pull", Actor: "worker"})
				require.NoError(t, err)
				require.NoError(t, store.RevokeFederationEnrollment(ctx, current.Enrollment.ID))
				params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: current.Enrollment.ID}
			}
			result, err := store.IngestFederationEvents(ctx, params)
			if kind == "malformed-comment" {
				require.ErrorIs(t, err, db.ErrFederationIngestValidation, "independent payload validation remains atomic despite missing enrollment")
				after, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				require.Equal(t, before, after, "ordinary dependent validation still rolls back atomically")
				return
			}
			require.NoError(t, err, "only missing current-project enrollment is an event rejection")
			if hasDependent {
				require.Len(t, result.RejectedEvents, 2)
				require.Equal(t, params.Events[1].Event.EventUID, result.RejectedEvents[1].EventUID)
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
				if hasDependent {
					require.Equal(t, int64(3), result.PushCursorEventID)
				} else {
					require.Equal(t, int64(2), result.PushCursorEventID)
				}
				require.Equal(t, "Arrived", got.Title)
			}
			if kind == "mixed" || kind == "revoked-race" || hasDependent {
				current, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &target.ID, Capabilities: "push,pull", Actor: "worker"})
				require.NoError(t, err)
				params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: current.Enrollment.ID}
				retry, err := store.IngestFederationEvents(ctx, params)
				require.NoError(t, err)
				require.Empty(t, retry.RejectedEvents)
				require.Equal(t, len(result.RejectedEvents), retry.Accepted)
				require.Equal(t, 1, retry.Duplicates)
			}
			if kind == "dependent-on-rejected-create" {
				created, err := store.IssueByUID(ctx, freshUID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, "Must not be known", created.Title)
			}
			if kind == "rejected-move" {
				moved, err := store.IssueByUID(ctx, moved.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, source.ID, moved.ProjectID)
				require.Equal(t, "Must not acquire route authority", moved.Title)
			}
			if kind != "mixed" && kind != "revoked-race" && kind != "moved-peer" && !rejectedMove {
				got, err = store.IssueByID(ctx, moved.ID)
				require.NoError(t, err)
				require.Equal(t, moved.Title, got.Title)
			}
		})
	}
	return nil
}

// The adoption author grant is one-shot: missing enrollment cannot consume its
// marker while retaining a fresh snapshot that must be deliverable after join.
func checkRejectedAdoptionBaseline(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, target, moved, _ := federatedRehomeFixture(t, store, "baseline-rejection")
	_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: moved.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: moved.Revision, Actor: "worker"})
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "worker", AllowAdoptionSnapshotAuthors: true})
	require.NoError(t, err)
	snapshotUID, err := uid.New()
	require.NoError(t, err)
	payload, err := json.Marshal(map[string]any{"uid": snapshotUID, "title": "Historical linked issue", "author": "historical-author", "status": "open", "created_at": "2026-05-23T12:00:00.000Z", "links": []map[string]string{{"type": "related", "to_issue_uid": moved.UID}}})
	require.NoError(t, err)
	snapshot := newRemoteEvent(t, source, &snapshotUID, "issue.snapshot", "worker", origin, time.Now().UnixMilli(), payload)
	metadata := newRemoteEvent(t, source, nil, "project.metadata_updated", "worker", origin, time.Now().UnixMilli(), []byte(`{"project_uid":"`+source.UID+`","metadata":{"adopted":true}}`))
	params := db.FederationIngestParams{AllowEventRejections: true, ProjectID: source.ID, FederationEnrollmentID: grant.Enrollment.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, AllowSnapshotAuthorPreservation: true, AdoptionBaseline: db.FederationAdoptionBaselineComplete, AdoptionBaselineEndSourceEventID: 2, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: metadata}, {SourceEventID: 2, Event: snapshot}}}
	params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: 0}
	_, err = store.IngestFederationEvents(ctx, params)
	require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized, "a baseline with missing enrollment stays atomic")
	enrollment, err := federationEnrollmentByID(ctx, store, grant.Enrollment.ID)
	require.NoError(t, err)
	require.True(t, enrollment.AllowAdoptionSnapshotAuthors, "a failed baseline retains the one-shot grant")
	_, err = store.EventsByUIDs(ctx, source.ID, []string{metadata.EventUID})
	require.ErrorIs(t, err, db.ErrNotFound, "independent baseline metadata must roll back too")
	current, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &target.ID, Capabilities: "push,pull", Actor: "worker"})
	require.NoError(t, err)
	params.MoveSourceEnrollmentIDs = map[string]int64{target.UID: current.Enrollment.ID}
	result, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err, "joining allows the original baseline to retry")
	require.Equal(t, 2, result.Accepted)
	retained, err := store.IssueByUID(ctx, snapshotUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "historical-author", retained.Author)
	return nil
}
