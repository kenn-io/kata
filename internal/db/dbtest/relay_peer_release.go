package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayCommentReplyRelease checks a retained reply's target before offering it.
func RunRelayCommentReplyRelease(t *testing.T, store db.Storage, missing bool) {
	fixture := NewRelaySharedAccessFixture(t, store)
	ctx := db.WithRelayPeerEventFeatures(t.Context(), db.SupportedEventFeatures)
	binding := fixture.Grants[0].Enrollment.RelayBindingUID
	initial, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, initial)
	last := initial[len(initial)-1]
	require.NoError(t, store.AckRelayDeliveries(ctx, binding, 1, db.RelayStreamEvent, last.Sequence, last.Digest))

	targetUID, err := uid.New()
	require.NoError(t, err)
	if !missing {
		target, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: fixture.Issues[1].ID, Author: "member", Body: "Target"})
		require.NoError(t, err)
		targetUID = target.UID
	}
	commentUID, err := uid.New()
	require.NoError(t, err)
	spoke, err := uid.New()
	require.NoError(t, err)
	event := newRemoteEvent(t, fixture.Projects[0], &fixture.Issues[0].UID, "issue.commented", "member", spoke, 700,
		jsontext.Value(fmt.Sprintf(`{"comment_uid":%q,"author":"member","body":"Reply","created_at":"2026-05-23T12:01:00.000Z","reply_to_uid":%q,"reply_kind":"reply"}`, commentUID, targetUID)))
	require.Nil(t, event.RelatedIssueUID)
	safe, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: fixture.Issues[0].ID, Actor: "member", Patch: map[string]jsontext.Value{"test.note": jsontext.Value(`"safe"`)}})
	require.NoError(t, err)
	_, err = store.IngestFederationEvents(db.WithRootAttribution(ctx, fixture.Signer, "member"), db.FederationIngestParams{
		ProjectID: fixture.Projects[0].ID, SpokeInstanceUID: spoke, BoundActor: "member",
		Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}},
	})
	require.NoError(t, err)
	denied := errors.New("peer denied")
	restricted := db.WithProjectAccessTransactionCheck(ctx, func(_ context.Context, _ db.Transaction, projectUID string) error {
		if projectUID == fixture.Projects[1].UID {
			return denied
		}
		return nil
	})
	offered, err := store.PendingRelayDeliveries(restricted, binding, db.RelayStreamEvent, 100)
	if missing {
		require.ErrorIs(t, err, db.ErrRelayReferenceUnprovable)
	} else {
		require.ErrorIs(t, err, denied)
	}
	require.Empty(t, offered)
	if missing {
		return
	}
	offered, err = store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	var found, foundSafe bool
	for _, envelope := range offered {
		if envelope.SourceUID == safe.Event.UID {
			foundSafe = true
		}
		if envelope.SourceUID == event.EventUID {
			stored, err := db.DecodeRelaySourceEvent(envelope.Body)
			require.NoError(t, err)
			require.Equal(t, event, stored)
			found = true
		}
	}
	require.True(t, found)
	require.True(t, foundSafe)
	retry, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, offered, retry)
}

// RunRelayPeerRelease checks whole-response refusal and retained retries.
func RunRelayPeerRelease(t *testing.T, store db.Storage) {
	fixture := NewRelaySharedAccessFixture(t, store)
	binding := fixture.Grant.Enrollment.RelayBindingUID
	ctx := db.WithRelayPeerEventFeatures(t.Context(), db.SupportedEventFeatures)
	denied := errors.New("peer denied")
	rejecting := db.WithProjectAccessTransactionCheck(ctx, func(_ context.Context, _ db.Transaction, uid string) error {
		if uid == fixture.Projects[1].UID {
			return denied
		}
		return nil
	})
	state := func() []db.ImportRecord {
		var records []db.ImportRecord
		for record, err := range store.ExportRelayState(ctx) {
			require.NoError(t, err)
			records = append(records, record)
		}
		return records
	}
	before := state()
	grants, err := store.ListProjectFederationEnrollments(ctx, fixture.Projects[0].ID)
	require.NoError(t, err)
	out, err := store.PendingRelayDeliveries(rejecting, binding, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, denied)
	require.Empty(t, out)
	require.Equal(t, before, state())
	afterGrants, err := store.ListProjectFederationEnrollments(ctx, fixture.Projects[0].ID)
	require.NoError(t, err)
	require.Equal(t, grants, afterGrants)
	offered, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, offered)
	out, err = store.PendingRelayDeliveries(rejecting, binding, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, denied)
	require.Empty(t, out)
	retried, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, offered, retried)

	reset := store.(db.RelayResetStore)
	before = state()
	grants, err = store.ListProjectFederationEnrollments(ctx, fixture.Projects[0].ID)
	require.NoError(t, err)
	maxEvent, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	blocked, err := reset.CreateRelayReset(rejecting, binding, fixture.Signer)
	require.ErrorIs(t, err, denied)
	require.NotContains(t, err.Error(), fixture.Issues[1].UID)
	require.NotContains(t, err.Error(), fixture.Projects[1].UID)
	require.Equal(t, db.RelayResetCheckpoint{}, blocked)
	require.Equal(t, before, state())
	afterGrants, err = store.ListProjectFederationEnrollments(ctx, fixture.Projects[0].ID)
	require.NoError(t, err)
	require.Equal(t, grants, afterGrants)
	currentEvent, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, maxEvent, currentEvent)
	checkpoint, err := reset.CreateRelayReset(ctx, binding, fixture.Signer)
	require.NoError(t, err)
	public := fixture.Signer.PrivateKey.Public().(ed25519.PublicKey)
	pin := db.RootKeyPin{ProjectUID: fixture.Projects[0].UID, AuthorityUID: fixture.Signer.AuthorityUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
	before = state()
	blocked, err = reset.CreateRelayReset(rejecting, binding, fixture.Signer)
	require.ErrorIs(t, err, denied)
	require.Equal(t, db.RelayResetCheckpoint{}, blocked)
	require.Equal(t, before, state())
	repeated, err := reset.CreateRelayReset(ctx, binding, fixture.Signer)
	require.NoError(t, err)
	require.Equal(t, checkpoint, repeated)
	activated := db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch)
	_, err = store.PendingRelayDeliveries(activated, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	before = state()
	deniedActivated := db.WithProjectAccessTransactionCheck(activated, func(_ context.Context, _ db.Transaction, uid string) error {
		if uid == fixture.Projects[1].UID {
			return denied
		}
		return nil
	})
	blocked, err = reset.CreateRelayReset(deniedActivated, binding, fixture.Signer)
	require.ErrorIs(t, err, denied)
	require.Equal(t, db.RelayResetCheckpoint{}, blocked)
	require.Equal(t, before, state())
	repeated, err = reset.CreateRelayReset(activated, binding, fixture.Signer)
	require.NoError(t, err)
	require.Equal(t, checkpoint, repeated)
}

// RunRelayPurgedPeer checks the retained event against the purged peer's project.
func RunRelayPurgedPeer(t *testing.T, store db.Storage) {
	fixture := newRelaySharedAccessFixture(t, store, true)
	ctx := db.WithRelayPeerEventFeatures(t.Context(), db.SupportedEventFeatures)
	binding := fixture.Grant.Enrollment.RelayBindingUID
	denied := errors.New("peer denied")
	restricted := db.WithProjectAccessTransactionCheck(ctx, func(_ context.Context, _ db.Transaction, uid string) error {
		if uid == fixture.Projects[1].UID {
			return denied
		}
		return nil
	})
	out, err := store.PendingRelayDeliveries(restricted, binding, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, denied)
	require.Empty(t, out)
	out, err = store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, out)
	_, err = store.(db.RelayResetStore).CreateRelayReset(restricted, binding, fixture.Signer)
	require.ErrorIs(t, err, denied)
	_, err = store.(db.RelayResetStore).CreateRelayReset(ctx, binding, fixture.Signer)
	require.NoError(t, err)
}

// RunRelayMissingNotification refuses an event whose comment owner is gone.
func RunRelayMissingNotification(t *testing.T, store db.Storage, deleted bool) {
	fixture := NewRelaySharedAccessFixture(t, store)
	ctx := db.WithRelayPeerEventFeatures(t.Context(), db.SupportedEventFeatures)
	targetUID := "00000000000000000000000009"
	if deleted {
		project, err := store.CreateProject(ctx, "comment-target-project")
		require.NoError(t, err)
		target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "member", Title: "Comment target"})
		require.NoError(t, err)
		comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "member", Body: "Target"})
		require.NoError(t, err)
		targetUID = comment.UID
		_, err = store.PurgeIssue(ctx, target.ID, "member", nil)
		require.NoError(t, err)
	}
	_, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: fixture.Issues[0].ID, Actor: "member", Patch: map[string]jsontext.Value{"notify.reader": jsontext.Value(fmt.Sprintf(`{"re":%q}`, targetUID))}})
	require.NoError(t, err)
	before := func() []db.ImportRecord {
		var records []db.ImportRecord
		for record, err := range store.ExportRelayState(ctx) {
			require.NoError(t, err)
			records = append(records, record)
		}
		return records
	}()
	envelopes, err := store.PendingRelayDeliveries(ctx, fixture.Grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, db.ErrRelayReferenceUnprovable)
	require.Empty(t, envelopes)
	var after []db.ImportRecord
	for record, err := range store.ExportRelayState(ctx) {
		require.NoError(t, err)
		after = append(after, record)
	}
	require.Equal(t, before, after)
	_, err = store.(db.RelayResetStore).CreateRelayReset(ctx, fixture.Grant.Enrollment.RelayBindingUID, fixture.Signer)
	require.ErrorIs(t, err, db.ErrRelayReferenceUnprovable)
}
