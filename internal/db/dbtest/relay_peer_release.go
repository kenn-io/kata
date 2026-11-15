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
func RunRelayCommentReplyRelease(t *testing.T, store db.Storage) {
	fixture := NewRelaySharedAccessFixture(t, store)
	ctx := db.WithRelayPeerEventFeatures(t.Context(), db.SupportedEventFeatures)
	binding := fixture.Grant.Enrollment.RelayBindingUID
	initial, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, initial)
	last := initial[len(initial)-1]
	require.NoError(t, store.AckRelayDeliveries(ctx, binding, 1, db.RelayStreamEvent, last.Sequence, last.Digest))

	target, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: fixture.Issues[1].ID, Author: "member", Body: "Target"})
	require.NoError(t, err)
	targetUID := target.UID
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
	require.ErrorIs(t, err, denied)
	require.Empty(t, offered)
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
	last = offered[len(offered)-1]
	require.NoError(t, store.AckRelayDeliveries(ctx, binding, 1, db.RelayStreamEvent, last.Sequence, last.Digest))
	missingUID, err := uid.New()
	require.NoError(t, err)
	commentUID, err = uid.New()
	require.NoError(t, err)
	missingEvent := newRemoteEvent(t, fixture.Projects[0], &fixture.Issues[0].UID, "issue.commented", "member", spoke, 701,
		jsontext.Value(fmt.Sprintf(`{"comment_uid":%q,"author":"member","body":"Reply","created_at":"2026-05-23T12:01:00.000Z","reply_to_uid":%q,"reply_kind":"reply"}`, commentUID, missingUID)))
	_, err = store.IngestFederationEvents(db.WithRootAttribution(ctx, fixture.Signer, "member"), db.FederationIngestParams{
		ProjectID: fixture.Projects[0].ID, SpokeInstanceUID: spoke, BoundActor: "member",
		Events: []db.FederationIngestEvent{{SourceEventID: 2, Event: missingEvent}},
	})
	require.NoError(t, err)
	offered, err = store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, offered)
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
	_, err := store.(db.RelayResetStore).CreateRelayReset(t.Context(), binding, fixture.Signer)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	before := relayStateForBinding(t, store, binding)
	grants, err := store.ListProjectFederationEnrollments(ctx, fixture.Projects[0].ID)
	require.NoError(t, err)
	out, err := store.PendingRelayDeliveries(rejecting, binding, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, denied)
	require.Empty(t, out)
	require.Equal(t, before, relayStateForBinding(t, store, binding))
	afterGrants, err := store.ListProjectFederationEnrollments(ctx, fixture.Projects[0].ID)
	require.NoError(t, err)
	require.Equal(t, grants, afterGrants)
	offered, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, offered)
	var crossing db.RelayEnvelope
	for _, envelope := range offered {
		if envelope.SourceUID == fixture.Linked.UID {
			crossing = envelope
		}
	}
	require.Equal(t, fixture.Linked.ContentHash, crossing.SourceHash)
	out, err = store.PendingRelayDeliveries(rejecting, binding, db.RelayStreamEvent, 100)
	require.ErrorIs(t, err, denied)
	require.Empty(t, out)
	retried, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, offered, retried)

	reset := store.(db.RelayResetStore)
	before = relayStateForBinding(t, store, binding)
	grants, err = store.ListProjectFederationEnrollments(ctx, fixture.Projects[0].ID)
	require.NoError(t, err)
	maxEvent, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	blocked, err := reset.CreateRelayReset(rejecting, binding, fixture.Signer)
	require.ErrorIs(t, err, denied)
	require.NotContains(t, err.Error(), fixture.Issues[1].UID)
	require.NotContains(t, err.Error(), fixture.Projects[1].UID)
	require.Equal(t, db.RelayResetCheckpoint{}, blocked)
	require.Equal(t, before, relayStateForBinding(t, store, binding))
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
	before = relayStateForBinding(t, store, binding)
	blocked, err = reset.CreateRelayReset(rejecting, binding, fixture.Signer)
	require.ErrorIs(t, err, denied)
	require.Equal(t, db.RelayResetCheckpoint{}, blocked)
	require.Equal(t, before, relayStateForBinding(t, store, binding))
	repeated, err := reset.CreateRelayReset(ctx, binding, fixture.Signer)
	require.NoError(t, err)
	require.Equal(t, checkpoint, repeated)
	activated := db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch)
	_, err = store.PendingRelayDeliveries(activated, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	before = relayStateForBinding(t, store, binding)
	deniedActivated := db.WithProjectAccessTransactionCheck(activated, func(_ context.Context, _ db.Transaction, uid string) error {
		if uid == fixture.Projects[1].UID {
			return denied
		}
		return nil
	})
	blocked, err = reset.CreateRelayReset(deniedActivated, binding, fixture.Signer)
	require.ErrorIs(t, err, denied)
	require.Equal(t, db.RelayResetCheckpoint{}, blocked)
	require.Equal(t, before, relayStateForBinding(t, store, binding))
	repeated, err = reset.CreateRelayReset(activated, binding, fixture.Signer)
	require.NoError(t, err)
	require.Equal(t, checkpoint, repeated)
}
