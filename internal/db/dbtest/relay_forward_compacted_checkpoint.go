package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunForwardCompactedRelayCheckpointPostCheckpointIngress exercises a fresh
// child namespace after a relay installed a compacted root checkpoint and then
// accepted synchronized events and receipts beyond that checkpoint's baselines.
func RunForwardCompactedRelayCheckpointPostCheckpointIngress(t *testing.T, store db.Storage, compact func(context.Context, int64) error) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "forwarded-checkpoint-project")
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	rootPublic, rootPrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(rootPublic), PublicKey: rootPublic}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "relay-member", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	bindingUID, err := uid.New()
	require.NoError(t, err)
	config := db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, AuthorityUID: rootUID, UpstreamInstanceUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "relay-member", ServeDownstream: true, ResetEpoch: 18}
	_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
	require.NoError(t, err)

	issueUID, err := uid.New()
	require.NoError(t, err)
	initial := newRemoteEvent(t, project, &issueUID, "issue.created", "source-agent", rootUID, 300, jsontext.Value(`{"uid":"`+issueUID+`","title":"Checkpoint title","author":"source-agent","metadata":{}}`))
	initialBytes, err := db.EncodeRelaySourceEvent(initial)
	require.NoError(t, err)
	initialReceipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: project.UID, EventUID: initial.EventUID, ContentHash: initial.ContentHash, AuthorityUID: rootUID, AccountableActor: "root-member", SourceActor: initial.Actor, IngressInstanceUID: rootUID, AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: 1, KeyID: pin.KeyID}, rootPrivate)
	require.NoError(t, err)
	eventBytes, err := json.Marshal([][]byte{initialBytes})
	require.NoError(t, err)
	entityBytes, err := json.Marshal([][]byte{})
	require.NoError(t, err)
	provenanceBytes, err := json.Marshal(db.RootResetProvenance{
		Keys:     []db.RootKeyPin{pin},
		Receipts: []db.AttributionReceipt{initialReceipt},
		Entities: []db.EntityProvenance{{ProjectUID: project.UID, Kind: "issue", EntityUID: issueUID, EventUID: initial.EventUID}},
	})
	require.NoError(t, err)
	snapshot := db.RootResetSnapshot{Events: eventBytes, Entities: entityBytes, Provenance: provenanceBytes, Artifacts: []byte(`[]`)}
	snapshotUID, err := uid.New()
	require.NoError(t, err)
	manifest, err := db.SignRootResetManifest(db.RootResetManifest{
		Version: 1, ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: pin.KeyID,
		ResetEpoch: 3, SnapshotUID: snapshotUID,
		RootBaselines: db.RelayStreamCursors{Events: 12, Receipts: 22}, HistoryEventID: 100,
	}, snapshot, rootPrivate)
	require.NoError(t, err)
	upstreamTranslation := db.RelayResetTranslation{
		Authority:   db.RelayHopAuthority{BindingUID: bindingUID, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: config.ResetEpoch + 1},
		SnapshotUID: snapshotUID, SnapshotDigest: manifest.SnapshotDigest,
		HopBaselines: db.RelayStreamCursors{Events: 40, Receipts: 50},
	}
	installer := store.(interface {
		InstallRelayReset(context.Context, string, db.RootResetManifest, db.RootResetSnapshot, db.RelayResetTranslation) error
	})
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, upstreamTranslation))

	postIssueUID, err := uid.New()
	require.NoError(t, err)
	updated := newRemoteEvent(t, project, &issueUID, "issue.updated", "root-editor", rootUID, 301, jsontext.Value(`{"title":"Synchronized update","updated_at":"2026-05-23T12:00:00.000Z"}`))
	created := newRemoteEvent(t, project, &postIssueUID, "issue.created", "source-agent", rootUID, 302, jsontext.Value(`{"uid":"`+postIssueUID+`","title":"Synchronized creation","author":"source-agent","metadata":{}}`))
	sealEvent := func(source db.RemoteEvent, sequence int64) db.RelayEnvelope {
		body, err := db.EncodeRelaySourceEvent(source)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: bindingUID, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: config.ResetEpoch + 1, Sequence: sequence, Stream: db.RelayStreamEvent, Path: []string{rootUID}, SourceUID: source.EventUID, SourceHash: source.ContentHash, Body: body})
		require.NoError(t, err)
		return envelope
	}
	_, err = store.AcceptRelayDeliveries(ctx, bindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{sealEvent(updated, 41), sealEvent(created, 42)}})
	require.NoError(t, err)
	postReceipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: project.UID, EventUID: created.EventUID, ContentHash: created.ContentHash, AuthorityUID: rootUID, AccountableActor: "root-member", SourceActor: created.Actor, IngressInstanceUID: rootUID, AcceptedAt: time.Now().UTC(), ResetEpoch: 3, Sequence: 2, KeyID: pin.KeyID}, rootPrivate)
	require.NoError(t, err)
	receiptBytes, err := json.Marshal(postReceipt)
	require.NoError(t, err)
	receiptEnvelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: bindingUID, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: config.ResetEpoch + 1, Sequence: 51, Stream: db.RelayStreamReceipt, Path: []string{rootUID}, SourceUID: created.EventUID, SourceHash: created.ContentHash, Body: receiptBytes})
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(ctx, bindingUID, db.RelayBatch{Stream: db.RelayStreamReceipt, Envelopes: []db.RelayEnvelope{receiptEnvelope}})
	require.NoError(t, err)

	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "relay-member", AdminActor: "admin", PlaintextToken: "forwarded-checkpoint-parent-test-token"})
	require.NoError(t, err)
	downstreamUID := "00000000000000000000000008"
	downstream, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: downstreamUID, ProtocolVersion: db.RelayProtocolVersion, Token: "forwarded-checkpoint-downstream-test-token"})
	require.NoError(t, err)

	_, localEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Locally produced", Author: "relay-member"})
	require.NoError(t, err)
	localSource := db.RemoteEventFromStored(localEvent)
	downstreamIssueUID, err := uid.New()
	require.NoError(t, err)
	downstreamSource := newRemoteEvent(t, project, &downstreamIssueUID, "issue.created", "downstream-agent", downstreamUID, 303, jsontext.Value(`{"uid":"`+downstreamIssueUID+`","title":"Downstream ingress","author":"downstream-agent","metadata":{}}`))
	downstreamBody, err := db.EncodeRelaySourceEvent(downstreamSource)
	require.NoError(t, err)
	downstreamEnvelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: downstream.Enrollment.RelayBindingUID, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: downstreamUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: downstream.Enrollment.RelayResetEpoch, Sequence: 1, Stream: db.RelayStreamEvent, Path: []string{downstreamUID}, SourceUID: downstreamSource.EventUID, SourceHash: downstreamSource.ContentHash, Body: downstreamBody})
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(ctx, downstream.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{downstreamEnvelope}})
	require.NoError(t, err)
	require.NoError(t, compact(ctx, project.ID))

	child, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000009", ProtocolVersion: db.RelayProtocolVersion, Token: "forwarded-checkpoint-child-test-token"})
	require.NoError(t, err)
	bootstrap := store.(db.RelayResetBootstrapStore)
	required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, child.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.True(t, required, "compacted source history requires a forwarded checkpoint")
	forwarder := store.(interface {
		CreateRelayReset(context.Context, string, db.RootAttributionSigner) (db.RelayResetCheckpoint, error)
	})
	forwarded, err := forwarder.CreateRelayReset(ctx, child.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.Equal(t, manifest, forwarded.Manifest, "forwarding preserves the signed root manifest")
	require.Equal(t, snapshot, forwarded.Snapshot, "forwarding preserves the signed root snapshot")
	require.Equal(t, db.RelayStreamCursors{}, forwarded.Translation.HopBaselines, "the new child hop starts at its own baselines")

	childCtx := db.WithRelayRequestedEpoch(ctx, forwarded.Translation.Authority.Epoch)
	events, err := store.PendingRelayDeliveries(childCtx, child.Enrollment.RelayBindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Len(t, events, 4, "all retained post-checkpoint source events enter the child's new namespace")
	wantSources := map[string]struct {
		hash string
		body []byte
		path []string
	}{
		updated.EventUID:          {hash: updated.ContentHash, body: mustEncodeRelaySourceEvent(t, updated), path: []string{rootUID, store.InstanceUID()}},
		created.EventUID:          {hash: created.ContentHash, body: mustEncodeRelaySourceEvent(t, created), path: []string{rootUID, store.InstanceUID()}},
		localEvent.UID:            {hash: localEvent.ContentHash, body: mustEncodeRelaySourceEvent(t, localSource), path: []string{store.InstanceUID()}},
		downstreamSource.EventUID: {hash: downstreamSource.ContentHash, body: downstreamBody, path: []string{downstreamUID, store.InstanceUID()}},
	}
	gotEventUIDs := make([]string, 0, len(events))
	for _, envelope := range events {
		gotEventUIDs = append(gotEventUIDs, envelope.SourceUID)
		want, ok := wantSources[envelope.SourceUID]
		require.True(t, ok, "unexpected forwarded event %s", envelope.SourceUID)
		require.Equal(t, want.hash, envelope.SourceHash)
		require.Equal(t, want.body, envelope.Body, "forwarding preserves exact source bytes")
		require.Equal(t, want.path, envelope.Path, "forwarding preserves the source relay path")
	}
	require.ElementsMatch(t, []string{updated.EventUID, created.EventUID, localEvent.UID, downstreamSource.EventUID}, gotEventUIDs)
	receipts, err := store.PendingRelayDeliveries(childCtx, child.Enrollment.RelayBindingUID, db.RelayStreamReceipt, 10)
	require.NoError(t, err)
	require.Len(t, receipts, 1, "post-checkpoint root receipt enters the child's new namespace")
	require.Equal(t, created.EventUID, receipts[0].SourceUID)
	require.Equal(t, postReceipt, mustDecodeReceipt(t, receipts[0].Body))
}

func mustEncodeRelaySourceEvent(t *testing.T, event db.RemoteEvent) []byte {
	t.Helper()
	body, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	return body
}

func mustDecodeReceipt(t *testing.T, raw []byte) db.AttributionReceipt {
	t.Helper()
	var receipt db.AttributionReceipt
	require.NoError(t, json.Unmarshal(raw, &receipt))
	return receipt
}
