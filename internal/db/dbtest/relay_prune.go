package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayRevocationPrunesBindingState exercises the retention rule for relay
// delivery rows on a root hub with two downstream grants. Revoking one grant
// removes that binding's outbox, inbox and cursor rows in the same operation;
// the live sibling keeps every row unchanged and still receives work; and the
// root signs a verifiable checkpoint for the live binding after its own event
// history is compacted, because creation proofs live in the provenance tables
// rather than in the revoked binding's rows.
func RunRelayRevocationPrunesBindingState(t *testing.T, store db.Storage, compact func(context.Context, int64) error) {
	creator, ok := store.(db.RelayResetStore)
	require.True(t, ok, "native root checkpoint generation is required")
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "prune-root-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
	writeCtx := db.WithRootAttribution(ctx, signer, "company-member")
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "prune-parent-test-token", Actor: "company-member", AdminActor: "admin"})
	require.NoError(t, err)
	revokedPeer := "00000000000000000000000006"
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	revoked, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: revokedPeer, ProtocolVersion: db.RelayProtocolVersion, Token: "prune-revoked-test-token"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	live, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000007", ProtocolVersion: db.RelayProtocolVersion, Token: "prune-live-test-token"})
	require.NoError(t, err)
	revokedBinding, liveBinding := revoked.Enrollment.RelayBindingUID, live.Enrollment.RelayBindingUID

	rootIssue, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Root issue", Author: "source-assistant"})
	require.NoError(t, err)
	pushedUID, err := uid.New()
	require.NoError(t, err)
	pushed := newRemoteEvent(t, project, &pushedUID, "issue.created", "source-assistant", revokedPeer, 300, jsontext.Value(`{"uid":"`+pushedUID+`","title":"Pushed by the revoked peer","author":"source-assistant","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
	body, err := db.EncodeRelaySourceEvent(pushed)
	require.NoError(t, err)
	envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: revokedBinding, ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: revokedPeer, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: 1, Stream: db.RelayStreamEvent, Path: []string{revokedPeer}, SourceUID: pushed.EventUID, SourceHash: pushed.ContentHash, Body: body})
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(writeCtx, revokedBinding, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}})
	require.NoError(t, err)
	rootProof, err := store.EntityAttribution(ctx, project.UID, "issue", rootIssue.UID)
	require.NoError(t, err)
	pushedProof, err := store.EntityAttribution(ctx, project.UID, "issue", pushedUID)
	require.NoError(t, err)
	drainRelayBinding(t, store, revokedBinding, 1)
	drainRelayBinding(t, store, liveBinding, 1)

	kinds := map[string]int{}
	for _, record := range relayStateForBinding(t, store, revokedBinding) {
		kinds[record.ImportKind()]++
	}
	require.Positive(t, kinds["federation_relay_outbox"], "the revoked binding has emitted rows")
	require.Positive(t, kinds["federation_relay_inbox"], "the revoked binding has accepted rows")
	require.Positive(t, kinds["federation_relay_cursors"], "the revoked binding has cursor rows")
	before := relayStateForBinding(t, store, liveBinding)
	require.NotEmpty(t, before)

	require.NoError(t, store.RevokeFederationEnrollment(ctx, revoked.Enrollment.ID))
	require.Empty(t, relayStateForBinding(t, store, revokedBinding), "revocation removes the binding's outbox, inbox and cursor rows")
	require.Equal(t, before, relayStateForBinding(t, store, liveBinding), "a live sibling binding keeps every row")
	require.NoError(t, store.RevokeFederationEnrollment(ctx, revoked.Enrollment.ID), "repeated revocation stays idempotent")
	require.Equal(t, before, relayStateForBinding(t, store, liveBinding))
	_, err = store.PendingRelayDeliveries(ctx, revokedBinding, db.RelayStreamEvent, 10)
	require.Error(t, err, "a revoked binding no longer delivers")

	_, _, err = store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "After revocation", Author: "source-assistant"})
	require.NoError(t, err)
	pending, err := store.PendingRelayDeliveries(ctx, liveBinding, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the live binding keeps receiving new work")
	require.Empty(t, relayStateForBinding(t, store, revokedBinding), "new work is not queued for a revoked binding")
	drainRelayBinding(t, store, liveBinding, 1)

	require.NoError(t, compact(ctx, project.ID))
	checkpoint, err := creator.CreateRelayReset(ctx, liveBinding, signer)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
	require.NoError(t, db.ValidateRelayResetTranslation(checkpoint.Translation.Authority, checkpoint.Manifest, checkpoint.Translation))
	require.Equal(t, live.Enrollment.SpokeInstanceUID, checkpoint.Translation.Authority.ReceiverInstanceUID)
	decoded, provenance, err := db.DecodeRootResetPayload(pin, checkpoint.Snapshot)
	require.NoError(t, err)
	issues := map[string]struct{}{}
	for _, event := range decoded {
		if event.IssueUID != nil {
			issues[*event.IssueUID] = struct{}{}
		}
	}
	require.Contains(t, issues, rootIssue.UID)
	require.Contains(t, issues, pushedUID, "work accepted from the revoked peer stays in the project")
	require.Contains(t, provenance.Receipts, rootProof)
	require.Contains(t, provenance.Receipts, pushedProof, "the receipt for the revoked peer's work survives in the provenance tables")
	require.Contains(t, provenance.Entities, db.EntityProvenance{ProjectUID: project.UID, Kind: "issue", EntityUID: pushedUID, EventUID: pushedProof.EventUID})
}

// drainRelayBinding emits and acknowledges every pending delivery on all
// three streams of one binding.
func drainRelayBinding(t *testing.T, store db.Storage, bindingUID string, epoch int64) {
	t.Helper()
	ctx := t.Context()
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact} {
		for {
			batch, err := store.PendingRelayDeliveries(ctx, bindingUID, stream, 32)
			require.NoError(t, err)
			if len(batch) == 0 {
				break
			}
			last := batch[len(batch)-1]
			require.NoError(t, store.AckRelayDeliveries(ctx, bindingUID, epoch, stream, last.Sequence, last.Digest))
		}
	}
}
