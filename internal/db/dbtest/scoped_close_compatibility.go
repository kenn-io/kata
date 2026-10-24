package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunScopedCloseNativeEnvelopeAdmission checks the actual hub ingest boundary,
// with a native enrollment and a successful typed introduction first. A current
// close proof cannot name a writer later than the envelope carrying that state.
func RunScopedCloseNativeEnvelopeAdmission(t *testing.T, store db.Storage) {
	t.Helper()
	for _, kind := range []string{"issue.created", "issue.snapshot"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			project, err := store.CreateProject(ctx, "envelope-"+strings.TrimPrefix(kind, "issue."))
			require.NoError(t, err)
			_, err = store.EnableProjectFederation(ctx, project.ID, "member")
			require.NoError(t, err)
			origin, err := uid.New()
			require.NoError(t, err)
			enrollment, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{Token: "close-envelope-token-" + kind, SpokeInstanceUID: origin, ProjectID: &project.ID, Capabilities: "push", Actor: "member"})
			require.NoError(t, err)
			makeEvent := func(futureProof bool) db.RemoteEvent {
				eventUID, err := uid.New()
				require.NoError(t, err)
				subject, err := uid.New()
				require.NoError(t, err)
				writer, err := uid.New()
				require.NoError(t, err)
				now := time.Now().UTC()
				envelopeMS := now.UnixMilli() + 1000
				clock := db.FoldClock{EventUID: writer, OriginInstanceUID: origin, HLCPhysicalMS: envelopeMS - 1}
				if futureProof {
					clock.HLCPhysicalMS = envelopeMS + 1
				}
				proof := db.CanonicalCloseState{IssueUID: subject, StatusIntentUID: writer, StatusClock: clock, CloseBoundaryClock: &clock}
				require.NoError(t, db.ValidateScopedCanonicalCloseState(proof, subject, "closed"), "both carriers have valid shape; only envelope ordering differs")
				raw, err := json.Marshal(map[string]any{"uid": subject, "short_id": strings.ToLower(subject[len(subject)-6:]), "title": "Imported closed work", "author": "member", "status": "closed", "closed_reason": "done", "closed_at": now.Format(db.EventTimestampFormat), "created_at": now.Format(db.EventTimestampFormat), "updated_at": now.Format(db.EventTimestampFormat), "metadata": map[string]any{}, "close_state": proof})
				require.NoError(t, err)
				event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: project.UID, ProjectName: project.Name, IssueUID: &subject, Type: kind, Actor: "member", HLCPhysicalMS: envelopeMS, CreatedAt: now, Payload: raw}
				event.ContentHash, err = db.RemoteEventContentHash(event)
				require.NoError(t, err)
				return event
			}
			params := db.FederationIngestParams{ProjectID: project.ID, FederationEnrollmentID: enrollment.Enrollment.ID, SpokeInstanceUID: origin, BoundActor: "member", EventFeatures: db.SupportedEventFeatures}
			valid := makeEvent(false)
			params.Events = []db.FederationIngestEvent{{SourceEventID: 1, Event: valid}}
			accepted, err := store.IngestFederationEvents(ctx, params)
			require.NoError(t, err, "valid typed introduction must reach native materialization first")
			require.Equal(t, 1, accepted.Accepted)
			require.Equal(t, int64(1), accepted.PushCursorEventID)
			introduced, err := store.IssueByUID(ctx, *valid.IssueUID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "closed", introduced.Status)
			before := scopedAuthoritySnapshot(t, store)
			require.NoError(t, db.ValidateImportRecords(before))
			invalid := makeEvent(true)
			params.Events = []db.FederationIngestEvent{{SourceEventID: 2, Event: invalid}}
			refused, err := store.IngestFederationEvents(ctx, params)
			require.ErrorIs(t, err, db.ErrFederationIngestValidation, "refuse future close writers before accepting an introduction the native fold would skip")
			require.Zero(t, refused.Accepted)
			require.Zero(t, refused.PushCursorEventID, "a rejected push must not acknowledge the malformed source row")
			_, err = store.IssueByUID(ctx, *invalid.IssueUID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound)
			require.Equal(t, before, scopedAuthoritySnapshot(t, store), "rejected native source leaves the complete owner snapshot unchanged")
		})
	}
	t.Run("envelope_only_self_close", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "envelope-subject-project")
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, project.ID, "member")
		require.NoError(t, err)
		subject, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "member", Title: "Close subject"})
		require.NoError(t, err)
		peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "member", Title: "Close peer"})
		require.NoError(t, err)
		origin, err := uid.New()
		require.NoError(t, err)
		enrollment, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{Token: "envelope-subject-token", SpokeInstanceUID: origin, ProjectID: &project.ID, Capabilities: "push", Actor: "member"})
		require.NoError(t, err)
		params := db.FederationIngestParams{ProjectID: project.ID, FederationEnrollmentID: enrollment.Enrollment.ID, SpokeInstanceUID: origin, BoundActor: "member", EventFeatures: db.SupportedEventFeatures}
		for n, peerUID := range []string{peer.UID, subject.UID} {
			eventUID, err := uid.New()
			require.NoError(t, err)
			raw, err := json.Marshal(map[string]any{"reason": "duplicate", "close_value": db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: project.UID, PeerIssueUID: peerUID}})
			require.NoError(t, err)
			now := time.Now().UTC()
			event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: project.UID, ProjectName: project.Name, IssueUID: &subject.UID, Type: "issue.closed", Actor: "member", HLCPhysicalMS: now.UnixMilli() + 1000 + int64(n), CreatedAt: now, Payload: raw}
			event.ContentHash, err = db.RemoteEventContentHash(event)
			require.NoError(t, err)
			params.Events = []db.FederationIngestEvent{{SourceEventID: int64(n + 1), Event: event}}
			before := scopedAuthoritySnapshot(t, store)
			result, err := store.IngestFederationEvents(ctx, params)
			if n == 0 {
				require.NoError(t, err, "ordinary typed close identifies its subject only in the native envelope")
				require.Equal(t, 1, result.Accepted)
				current, err := store.IssueByUID(ctx, subject.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, "closed", current.Status, "the valid native close actually materializes")
				continue
			}
			require.ErrorIs(t, err, db.ErrFederationIngestValidation, "envelope subject cannot be its own typed close peer")
			require.Zero(t, result.Accepted)
			require.Zero(t, result.PushCursorEventID)
			require.Equal(t, before, scopedAuthoritySnapshot(t, store), "self-close refusal preserves the complete native owner")
		}
	})
}

// RunScopedCloseLegacyNativeAdmission proves real legacy ingest/materialization
// accepts carrier-absent label extensions and retains the exact source bytes.
func RunScopedCloseLegacyNativeAdmission(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "member")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Example work", Author: "member"})
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	eventUID, err := uid.New()
	require.NoError(t, err)
	raw := jsontext.Value(`{ "label":"ready", "uid":7, "status":9, "reason":{"detail":"legacy-extension"} }`)
	now := time.Now().UTC()
	event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: project.UID, ProjectName: project.Name, IssueUID: &issue.UID, Type: "issue.labeled", Actor: "member", HLCPhysicalMS: now.UnixMilli() + 1000, CreatedAt: now, Payload: raw}
	event.ContentHash, err = db.RemoteEventContentHash(event)
	require.NoError(t, err)
	result, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: origin, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
	require.NoError(t, err, "native carrier-absent label extension must keep legacy admission")
	require.Equal(t, 1, result.Accepted)
	labels, err := store.LabelsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, labels, 1)
	require.Equal(t, "ready", labels[0].Label)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 20})
	require.NoError(t, err)
	found := false
	for _, stored := range events {
		if stored.UID == eventUID {
			found = true
			require.Equal(t, []byte(raw), []byte(stored.Payload), "native original bytes remain exact")
			require.Equal(t, event.ContentHash, stored.ContentHash)
		}
	}
	require.True(t, found, "native admitted source retained")
	require.Equal(t, `{ "label":"ready", "uid":7, "status":9, "reason":{"detail":"legacy-extension"} }`, string(event.Payload))
	t.Log("real native ingest accepted carrier-absent extensions, materialized label, and retained exact bytes/hash")
}

// RunScopedCloseLegacyMetadataNativeAdmission characterizes known native
// metadata events, never unsupported event families. Non-object payloads have
// no top-level reserved carrier and retain their native no-op meaning.
func RunScopedCloseLegacyMetadataNativeAdmission(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "metadata-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "member")
	require.NoError(t, err)
	before, err := store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	for n, raw := range []jsontext.Value{jsontext.Value(` 9 `), jsontext.Value(` [ 7, {"status":9} ] `)} {
		eventUID, err := uid.New()
		require.NoError(t, err)
		now := time.Now().UTC()
		event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: project.UID, ProjectName: project.Name, Type: "project.metadata_updated", Actor: "member", HLCPhysicalMS: now.UnixMilli() + 1000 + int64(n), CreatedAt: now, Payload: raw}
		event.ContentHash, err = db.RemoteEventContentHash(event)
		require.NoError(t, err, "native hash accepts this JSON value")
		result, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: origin, Events: []db.FederationIngestEvent{{SourceEventID: int64(n + 1), Event: event}}})
		require.NoError(t, err, "known native metadata carrier-absent payload %s", raw)
		require.Equal(t, 1, result.Accepted)
		after, err := store.ProjectByID(ctx, project.ID)
		require.NoError(t, err)
		require.Equal(t, before.Metadata, after.Metadata, "absent metadata diff remains a no-op")
		events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 20})
		require.NoError(t, err)
		found := false
		for _, stored := range events {
			if stored.UID == eventUID {
				found = true
				require.Equal(t, []byte(raw), []byte(stored.Payload), "retained non-object source bytes remain exact")
				require.Equal(t, event.ContentHash, stored.ContentHash)
			}
		}
		require.True(t, found)
		t.Logf("known metadata native admission retained non-object payload %s with exact bytes/hash and unchanged metadata", raw)
	}
}
