package dbtest

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayIngressAtomicity exercises relay ingress atomicity on the supplied native store.
// R3/R4/R5: narrowed transport admission, immutable third-party source identity,
// root accountability, projections and onward delivery commit atomically.
func RunRelayIngressAtomicity(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "root-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "company-member", AdminActor: "admin", PlaintextToken: "ingress-parent-test-token"})
	require.NoError(t, err)
	peer := "00000000000000000000000006"
	leaf := "00000000000000000000000007"
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion, Token: "ingress-relay-test-token", ServeDownstream: true})
	require.NoError(t, err)
	sibling, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000009", ProtocolVersion: db.RelayProtocolVersion, Token: "ingress-sibling-test-token"})
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	commentUID, err := uid.New()
	require.NoError(t, err)
	created := newRemoteEvent(t, project, &issueUID, "issue.created", "source-assistant", leaf, 300, jsontext.Value(`{"uid":"`+issueUID+`","title":"Relayed source task","body":"source body","author":"source-assistant","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
	commented := newRemoteEvent(t, project, &issueUID, "issue.commented", "comment-assistant", leaf, 301, jsontext.Value(`{"comment_uid":"`+commentUID+`","author":"comment-assistant","teammate":"reviewer","body":"Original comment","created_at":"2026-05-23T12:01:00.000Z"}`))
	seal := func(event db.RemoteEvent, sequence int64) db.RelayEnvelope {
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: peer, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: sequence, Stream: db.RelayStreamEvent, Path: []string{leaf, peer}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
		require.NoError(t, err)
		return envelope
	}
	batch := db.RelayBatch{Stream: db.RelayStreamEvent, After: 0, Envelopes: []db.RelayEnvelope{seal(created, 31), seal(commented, 40)}}
	signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
	writeCtx := db.WithRootAttribution(ctx, signer, "body-label-does-not-grant-authority")
	accepted, err := store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, batch)
	require.NoError(t, err)
	require.Equal(t, int64(40), accepted.Through)
	require.Equal(t, batch.Envelopes[1].Digest, accepted.Digest)
	require.Len(t, accepted.InsertedEventUIDs, 2)
	require.Len(t, accepted.InsertedEvents, 2)
	require.Equal(t, []string{created.EventUID, commented.EventUID}, []string{accepted.InsertedEvents[0].UID, accepted.InsertedEvents[1].UID})
	require.Equal(t, project.ID, accepted.InsertedEvents[0].ProjectID)
	require.Equal(t, "issue.created", accepted.InsertedEvents[0].Type)
	issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "source-assistant", issue.Author)
	require.Equal(t, "company-member", issue.AccountableActor)
	comments, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "comment-assistant", comments[0].Author)
	require.Equal(t, "reviewer", comments[0].Teammate)
	require.Equal(t, "company-member", comments[0].AccountableActor)
	proof, err := store.EntityAttribution(ctx, project.UID, "issue", issueUID)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootReceipt(pin, proof))
	require.Equal(t, "company-member", proof.AccountableActor)
	require.Equal(t, peer, proof.IngressInstanceUID)
	forwarded, err := store.PendingRelayDeliveries(ctx, sibling.Enrollment.RelayBindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Len(t, forwarded, 2)
	require.Equal(t, created.EventUID, forwarded[0].SourceUID)
	require.Equal(t, created.ContentHash, forwarded[0].SourceHash)
	require.Equal(t, []string{leaf, peer, store.InstanceUID()}, forwarded[0].Path)
	original, err := db.DecodeRelaySourceEvent(forwarded[0].Body)
	require.NoError(t, err)
	require.Equal(t, created, original)
	echoed, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Empty(t, echoed)
	proofs, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamReceipt, 10)
	require.NoError(t, err)
	require.Len(t, proofs, 2)
	require.Equal(t, []string{store.InstanceUID()}, proofs[0].Path)
	retry, err := store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, batch)
	require.NoError(t, err)
	require.Equal(t, accepted.Through, retry.Through)
	require.Equal(t, accepted.Digest, retry.Digest)
	require.Empty(t, retry.InsertedEventUIDs)
	require.Empty(t, retry.InsertedEvents, "replayed acceptance must not republish previously committed events")
	duplicateSource := seal(created, 45)
	_, err = store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{
		Stream: db.RelayStreamEvent, After: 40, Envelopes: []db.RelayEnvelope{duplicateSource},
	})
	require.ErrorIs(t, err, db.ErrFederationIngestValidation, "a source UID cannot be offered at a second sequence in the same epoch")
	wrong := batch
	wrong.After = 100
	_, err = store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, wrong)
	require.Error(t, err, "cannot skip a receiver's accepted prefix")
	tampered := created
	tampered.Actor = "impostor"
	tampered.ContentHash = remoteEventHash(t, tampered)
	_, err = store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, After: 40, Envelopes: []db.RelayEnvelope{seal(tampered, 50)}})
	require.Error(t, err)
	_, wrongPrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	failedUID, err := uid.New()
	require.NoError(t, err)
	failed := newRemoteEvent(t, project, &failedUID, "issue.created", "assistant", leaf, 302, jsontext.Value(`{"uid":"`+failedUID+`","title":"Must roll back","author":"assistant","metadata":{}}`))
	failedBatch := db.RelayBatch{Stream: db.RelayStreamEvent, After: 40, Envelopes: []db.RelayEnvelope{seal(failed, 51)}}
	_, err = store.AcceptRelayDeliveries(db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: wrongPrivate}, "company-member"), grant.Enrollment.RelayBindingUID, failedBatch)
	require.Error(t, err)
	_, err = store.IssueByUID(ctx, failedUID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	successful, err := store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, failedBatch)
	require.NoError(t, err)
	require.Equal(t, int64(51), successful.Through)
	require.Len(t, successful.InsertedEvents, 1)
	require.Equal(t, failed.EventUID, successful.InsertedEvents[0].UID)
	_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, batch)
	require.Error(t, err, "replay rechecks the current human credential")
}
