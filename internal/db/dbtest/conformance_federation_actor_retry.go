package dbtest

import (
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A re-enrolled spoke may reoffer immutable history accepted under its earlier
// actor. Only exact retained events may cross that actor boundary.
func checkFederationIngestActorRebindRetry(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "operator")
	require.NoError(t, err)
	spokeUID, err := uid.New()
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	commentUID, err := uid.New()
	require.NoError(t, err)
	history := []db.FederationIngestEvent{}
	for i, input := range []struct{ kind, payload string }{
		{"issue.created", `{"uid":"` + issueUID + `","title":"original","author":"earlier-worker","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`},
		{"issue.commented", `{"comment_uid":"` + commentUID + `","author":"earlier-worker","teammate":"builder","body":"retained work","created_at":"2026-05-23T12:01:00.000Z"}`},
		{"issue.metadata_updated", `{"diff":{"review.state":{"from":null,"to":"ready"}},"revision_new":3}`},
		{"issue.assigned", `{"owner":"earlier-worker"}`},
		{"issue.closed", `{"reason":"done","closed_at":"2026-05-23T12:02:00.000Z"}`},
	} {
		history = append(history, db.FederationIngestEvent{SourceEventID: int64(10 + i),
			Event: newRemoteEvent(t, project, &issueUID, input.kind, "earlier-worker", spokeUID,
				int64(300+i), jsontext.Value(input.payload))})
	}
	params := db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: spokeUID,
		BoundActor: "earlier-worker", Events: history}
	seeded, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	require.Equal(t, len(history), seeded.Accepted)
	historicalUIDs := make([]string, len(history))
	for i, input := range history {
		historicalUIDs[i] = input.Event.EventUID
	}
	before, err := store.EventsByUIDs(ctx, project.ID, historicalUIDs)
	require.NoError(t, err)
	closed, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)
	commentsBefore, err := store.CommentsByIssue(ctx, closed.ID)
	require.NoError(t, err)
	require.Len(t, commentsBefore, 1)
	require.Equal(t, "earlier-worker", commentsBefore[0].Author)
	require.Equal(t, "builder", commentsBefore[0].Teammate)

	// Replay the old prefix and its repeated comment before new-actor work.
	// The duplicate close must not override the later reopen on any retry.
	params.BoundActor = "current-worker"
	params.Events = append(append([]db.FederationIngestEvent{}, history...),
		db.FederationIngestEvent{SourceEventID: 15, Event: history[1].Event})
	reopened := newRemoteEvent(t, project, &issueUID, "issue.reopened", "current-worker", spokeUID, 400,
		jsontext.Value(`{"reopened_at":"2026-05-23T12:03:00.000Z"}`))
	params.Events = append(params.Events, db.FederationIngestEvent{SourceEventID: 20, Event: reopened})
	replayed, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	require.Equal(t, 1, replayed.Accepted)
	require.Equal(t, len(history)+1, replayed.Duplicates)
	require.Equal(t, int64(20), replayed.PushCursorEventID)
	require.Equal(t, []string{reopened.EventUID}, replayed.InsertedEventUIDs)
	require.Len(t, replayed.Events, 1, "historical duplicates must not be rebroadcast")
	after, err := store.EventsByUIDs(ctx, project.ID, historicalUIDs)
	require.NoError(t, err)
	require.Equal(t, before, after, "retain event identities, actors, hashes, payloads and ordering")
	commentsAfter, err := store.CommentsByIssue(ctx, closed.ID)
	require.NoError(t, err)
	require.Equal(t, commentsBefore, commentsAfter)
	current, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "open", current.Status)
	require.Equal(t, "earlier-worker", current.Author)
	require.Equal(t, closed.Metadata, current.Metadata)

	retry, err := store.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	require.Zero(t, retry.Accepted)
	require.Equal(t, len(params.Events), retry.Duplicates)
	require.Empty(t, retry.Events)

	for _, name := range []string{"fresh old actor", "changed payload", "invalid hash", "wrong origin", "wrong project"} {
		t.Run(name, func(t *testing.T) {
			invalid := history[1].Event
			wantErr := db.ErrFederationIngestValidation
			switch name {
			case "fresh old actor":
				invalid.EventUID, err = uid.New()
				require.NoError(t, err)
			case "changed payload":
				invalid.Payload = jsontext.Value(strings.ReplaceAll(string(invalid.Payload), "retained work", "changed work"))
				wantErr = db.ErrRemoteEventConflict
			case "invalid hash":
				wantErr = db.ErrRemoteEventHashMismatch
			case "wrong origin":
				invalid.OriginInstanceUID = store.InstanceUID()
			case "wrong project":
				invalid.ProjectUID, err = uid.New()
				require.NoError(t, err)
			}
			invalid.ContentHash = remoteEventHash(t, invalid)
			if name == "invalid hash" {
				invalid.ContentHash = strings.Repeat("0", 64)
			}
			fresh := newRemoteEvent(t, project, &issueUID, "issue.updated", "current-worker", spokeUID, 500,
				jsontext.Value(`{"title":"must roll back"}`))
			maxBefore, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			params.Events = []db.FederationIngestEvent{{SourceEventID: 21, Event: fresh}, {SourceEventID: 22, Event: invalid}}
			_, err = store.IngestFederationEvents(ctx, params)
			require.ErrorIs(t, err, wantErr)
			maxAfter, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, maxBefore, maxAfter, "rejected batch must not persist its preceding valid event")
			actual, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, current, actual)
		})
	}
	t.Run("old actor requires exact snapshot hash", func(t *testing.T) {
		// Trusted retained history may preserve another author's snapshot.
		// A consumed adoption retry must not normalize a changed old-actor
		// payload into that retained hash under the new enrollment's actor.
		snapshotIssueUID, err := uid.New()
		require.NoError(t, err)
		snapshot := newRemoteEvent(t, project, &snapshotIssueUID, "issue.snapshot", "earlier-worker", spokeUID, 600,
			jsontext.Value(`{"uid":"`+snapshotIssueUID+`","title":"snapshot","author":"current-worker","status":"open","metadata":{}}`))
		inserted, err := store.InsertRemoteEvent(ctx, project.ID, snapshot)
		require.NoError(t, err)
		require.True(t, inserted)
		enrollment, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			SpokeInstanceUID: spokeUID, ProjectID: &project.ID, Actor: "current-worker", Capabilities: "push",
		})
		require.NoError(t, err)
		changed := snapshot
		changed.Payload = jsontext.Value(strings.ReplaceAll(string(changed.Payload), "current-worker", "other-worker"))
		changed.ContentHash = remoteEventHash(t, changed)
		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID: project.ID, SpokeInstanceUID: spokeUID, BoundActor: "current-worker",
			FederationEnrollmentID: enrollment.Enrollment.ID, AdoptionBaseline: db.FederationAdoptionBaselineComplete,
			AdoptionBaselineEndSourceEventID: 100, Events: []db.FederationIngestEvent{{SourceEventID: 100, Event: changed}},
		})
		require.ErrorIs(t, err, db.ErrRemoteEventConflict)
	})
	return nil
}
