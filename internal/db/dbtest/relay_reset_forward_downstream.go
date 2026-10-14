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

// rootResetFixture signs root checkpoints and deliveries for a relay whose
// pinned root is an external authority the test controls. Like a real root,
// it keeps every receipt it ever signed: an issue purge removes the issue from
// later snapshots but never its receipt.
type rootResetFixture struct {
	t        *testing.T
	project  db.Project
	pin      db.RootKeyPin
	private  ed25519.PrivateKey
	rootUID  string
	relayUID string
	binding  string
	receipts []db.AttributionReceipt
	// historyEventID is the root-local replay boundary each checkpoint
	// declares; zero means a boundary far above the relay's own event ids.
	historyEventID int64
}

func (f *rootResetFixture) boundary() int64 {
	if f.historyEventID > 0 {
		return f.historyEventID
	}
	return 1000
}

type rootFixtureIssue struct {
	event   db.RemoteEvent
	receipt db.AttributionReceipt
	entity  db.EntityProvenance
}

func (f *rootResetFixture) created(title string, physicalMS, sequence int64) rootFixtureIssue {
	f.t.Helper()
	issueUID, err := uid.New()
	require.NoError(f.t, err)
	event := newRemoteEvent(f.t, f.project, &issueUID, "issue.created", "source-agent", f.rootUID, physicalMS, jsontext.Value(`{"uid":"`+issueUID+`","title":"`+title+`","author":"source-agent","metadata":{}}`))
	receipt := f.receipt(event, sequence)
	return rootFixtureIssue{event: event, receipt: receipt, entity: db.EntityProvenance{ProjectUID: f.project.UID, Kind: "issue", EntityUID: issueUID, EventUID: event.EventUID}}
}

func (f *rootResetFixture) receipt(event db.RemoteEvent, sequence int64) db.AttributionReceipt {
	f.t.Helper()
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: f.project.UID, EventUID: event.EventUID, ContentHash: event.ContentHash, AuthorityUID: f.rootUID, AccountableActor: "root-member", SourceActor: event.Actor, IngressInstanceUID: f.rootUID, AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: sequence, KeyID: f.pin.KeyID}, f.private)
	require.NoError(f.t, err)
	f.receipts = append(f.receipts, receipt)
	return receipt
}

// checkpoint signs the root's current issues as one root epoch, with every
// receipt signed so far, and translates it for the relay's upstream hop epoch.
func (f *rootResetFixture) checkpoint(rootEpoch, hopEpoch int64, issues ...rootFixtureIssue) (db.RootResetManifest, db.RootResetSnapshot, db.RelayResetTranslation) {
	f.t.Helper()
	sources := [][]byte{}
	provenance := db.RootResetProvenance{Keys: []db.RootKeyPin{f.pin}, Receipts: append([]db.AttributionReceipt{}, f.receipts...), Entities: []db.EntityProvenance{}}
	for _, issue := range issues {
		body, err := db.EncodeRelaySourceEvent(issue.event)
		require.NoError(f.t, err)
		sources = append(sources, body)
		provenance.Entities = append(provenance.Entities, issue.entity)
	}
	var snapshot db.RootResetSnapshot
	var err error
	snapshot.Events, err = json.Marshal(sources)
	require.NoError(f.t, err)
	snapshot.Entities, err = json.Marshal([][]byte{})
	require.NoError(f.t, err)
	snapshot.Provenance, err = json.Marshal(provenance)
	require.NoError(f.t, err)
	snapshot.Artifacts = []byte(`[]`)
	snapshotUID, err := uid.New()
	require.NoError(f.t, err)
	manifest, err := db.SignRootResetManifest(db.RootResetManifest{Version: 1, ProjectUID: f.project.UID, AuthorityUID: f.rootUID, KeyID: f.pin.KeyID, ResetEpoch: rootEpoch, SnapshotUID: snapshotUID, HistoryEventID: f.boundary()}, snapshot, f.private)
	require.NoError(f.t, err)
	translation := db.RelayResetTranslation{Authority: db.RelayHopAuthority{BindingUID: f.binding, ProjectUID: f.project.UID, AuthorityUID: f.rootUID, SenderInstanceUID: f.rootUID, ReceiverInstanceUID: f.relayUID, Epoch: hopEpoch}, SnapshotUID: snapshotUID, SnapshotDigest: manifest.SnapshotDigest}
	return manifest, snapshot, translation
}

// deliver pushes one root-origin envelope over the relay's upstream hop.
func (f *rootResetFixture) deliver(ctx context.Context, store db.Storage, stream string, hopEpoch, sequence int64, sourceUID, sourceHash string, body []byte) {
	f.t.Helper()
	envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: f.binding, ProjectUID: f.project.UID, AuthorityUID: f.rootUID, SenderInstanceUID: f.rootUID, ReceiverInstanceUID: f.relayUID, Epoch: hopEpoch, Sequence: sequence, Stream: stream, Path: []string{f.rootUID}, SourceUID: sourceUID, SourceHash: sourceHash, Body: body})
	require.NoError(f.t, err)
	_, err = store.AcceptRelayDeliveries(ctx, f.binding, db.RelayBatch{Stream: stream, Envelopes: []db.RelayEnvelope{envelope}})
	require.NoError(f.t, err)
}

func (f *rootResetFixture) deliverIssue(ctx context.Context, store db.Storage, issue rootFixtureIssue, hopEpoch, sequence int64) {
	f.t.Helper()
	body, err := db.EncodeRelaySourceEvent(issue.event)
	require.NoError(f.t, err)
	f.deliver(ctx, store, db.RelayStreamEvent, hopEpoch, sequence, issue.event.EventUID, issue.event.ContentHash, body)
	f.deliverReceipt(ctx, store, issue.receipt, hopEpoch, sequence)
}

func (f *rootResetFixture) deliverReceipt(ctx context.Context, store db.Storage, receipt db.AttributionReceipt, hopEpoch, sequence int64) {
	f.t.Helper()
	raw, err := json.Marshal(receipt)
	require.NoError(f.t, err)
	f.deliver(ctx, store, db.RelayStreamReceipt, hopEpoch, sequence, receipt.EventUID, receipt.ContentHash, raw)
}

// drainRelayGrant acknowledges every pending delivery on every stream for a
// grant in its current epoch, leaving it fully synchronized.
func drainRelayGrant(t *testing.T, store db.Storage, bindingUID string, epoch int64) {
	t.Helper()
	ctx := t.Context()
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact} {
		for {
			items, err := store.PendingRelayDeliveries(ctx, bindingUID, stream, 100)
			require.NoError(t, err)
			if len(items) == 0 {
				break
			}
			last := items[len(items)-1]
			require.NoError(t, store.AckRelayDeliveries(ctx, bindingUID, epoch, stream, last.Sequence, last.Digest))
		}
	}
}

// unacknowledgedRelayOutbox returns the retained outbox rows a grant has not
// acknowledged in one epoch, keyed by stream.
func unacknowledgedRelayOutbox(t *testing.T, store db.Storage, bindingUID string, epoch int64) map[string][]db.RelayOutboxExport {
	t.Helper()
	rows := map[string][]db.RelayOutboxExport{}
	for record, err := range store.ExportRelayState(t.Context()) {
		require.NoError(t, err)
		row, ok := record.(*db.RelayOutboxExport)
		if ok && row.BindingUID == bindingUID && row.ResetEpoch == epoch && !row.Acknowledged {
			rows[row.Stream] = append(rows[row.Stream], *row)
		}
	}
	return rows
}

func pendingSourceUIDs(ctx context.Context, t *testing.T, store db.Storage, bindingUID, stream string) []string {
	t.Helper()
	items, err := store.PendingRelayDeliveries(ctx, bindingUID, stream, 100)
	require.NoError(t, err)
	uids := make([]string, 0, len(items))
	for _, item := range items {
		uids = append(uids, item.SourceUID)
	}
	return uids
}

func supersededRecord(t *testing.T, store db.Storage, projectUID, bindingUID string) (db.RelayResetSupersededDeliveries, bool) {
	t.Helper()
	key := db.RelayResetSupersededMetadataKey(projectUID, bindingUID)
	for record, err := range store.ExportMeta(t.Context()) {
		require.NoError(t, err)
		if record.Key != key {
			continue
		}
		var superseded db.RelayResetSupersededDeliveries
		require.NoError(t, json.Unmarshal([]byte(record.Value), &superseded, json.RejectUnknownMembers(true)))
		return superseded, true
	}
	return db.RelayResetSupersededDeliveries{}, false
}

// RunRelayResetForwardsToActiveDownstream checks that a relay with live
// downstream enrollments installs a root reset by moving each enrollment to a
// forwarded checkpoint instead of refusing, that deliveries the checkpoint
// supersedes are recorded and never reoffered, and that a downstream holding
// work the root has not accepted still blocks installation.
func RunRelayResetForwardsToActiveDownstream(t *testing.T, store db.Storage) {
	ctx := t.Context()
	installer, ok := store.(db.RelayResetStore)
	require.True(t, ok, "native relay reset installation is required")
	bootstrap, ok := store.(db.RelayResetBootstrapStore)
	require.True(t, ok, "native reset detection is required")
	project, err := store.CreateProject(ctx, "relay-project")
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	rootPublic, rootPrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(rootPublic), PublicKey: rootPublic}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "relay-member", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	upstream, err := uid.New()
	require.NoError(t, err)
	_, err = store.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: upstream, AuthorityUID: rootUID, UpstreamInstanceUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "relay-member", ServeDownstream: true, ResetEpoch: 1})
	require.NoError(t, err)
	root := &rootResetFixture{t: t, project: project, pin: pin, private: rootPrivate, rootUID: rootUID, relayUID: store.InstanceUID(), binding: upstream}

	kept := root.created("Kept issue", 300, 1)
	purged := root.created("Purged at the root", 301, 2)
	manifest, snapshot, translation := root.checkpoint(2, 2, kept, purged)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "relay-member", AdminActor: "admin", PlaintextToken: "forward-downstream-parent-test-token"})
	require.NoError(t, err)
	deviceUID := "00000000000000000000000008"
	device, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: deviceUID, ProtocolVersion: db.RelayProtocolVersion, Token: "forward-downstream-device-test-token"})
	require.NoError(t, err)
	deviceBinding := device.Enrollment.RelayBindingUID
	drainRelayGrant(t, store, deviceBinding, device.Enrollment.RelayResetEpoch)

	// The root purged an issue: its next checkpoint omits it.
	manifest, snapshot, translation = root.checkpoint(3, 3, kept)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation), "a relay with a synchronized downstream installs the root reset")
	_, err = store.IssueByUID(ctx, purged.entity.EntityUID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, deviceBinding)
	require.NoError(t, err)
	require.True(t, required, "the downstream grant moved to a new epoch")
	forwarded, err := installer.CreateRelayReset(ctx, deviceBinding, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.Equal(t, manifest, forwarded.Manifest, "the forwarded checkpoint keeps the root's signed manifest")
	require.Equal(t, snapshot, forwarded.Snapshot)
	require.Equal(t, db.RelayHopAuthority{BindingUID: deviceBinding, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: store.InstanceUID(), ReceiverInstanceUID: deviceUID, Epoch: 2}, forwarded.Translation.Authority)
	require.Equal(t, db.RelayStreamCursors{}, forwarded.Translation.HopBaselines)
	deviceEpoch2 := db.WithRelayRequestedEpoch(ctx, 2)
	require.Empty(t, pendingSourceUIDs(deviceEpoch2, t, store, deviceBinding, db.RelayStreamEvent), "the snapshot carries the state; nothing is owed in the new epoch")
	required, err = bootstrap.RelayEnrollmentNeedsReset(ctx, deviceBinding)
	require.NoError(t, err)
	require.False(t, required, "the first new-epoch request activated the downstream grant")

	// A root issue reaches the relay and is queued to the device, which does
	// not acknowledge it before the root purges it again.
	transient := root.created("Transient", 302, 3)
	root.deliverIssue(ctx, store, transient, 3, 1)
	require.Contains(t, pendingSourceUIDs(ctx, t, store, deviceBinding, db.RelayStreamEvent), transient.event.EventUID)
	owed := unacknowledgedRelayOutbox(t, store, deviceBinding, 2)
	require.Len(t, owed[db.RelayStreamEvent], 1)
	require.NotEmpty(t, owed[db.RelayStreamReceipt])
	manifest, snapshot, translation = root.checkpoint(4, 4, kept)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation), "unacknowledged root-accepted deliveries are superseded, not blockers")
	retained := unacknowledgedRelayOutbox(t, store, deviceBinding, 2)
	require.Equal(t, owed, retained, "superseded rows stay in the outbox as history")
	var throughID int64
	for _, rows := range owed {
		for _, row := range rows {
			throughID = max(throughID, row.ID)
		}
	}
	record, found := supersededRecord(t, store, project.UID, deviceBinding)
	require.True(t, found, "installation records what it superseded")
	require.Equal(t, db.RelayResetSupersededDeliveries{Epoch: 2, NextEpoch: 3, SnapshotUID: manifest.SnapshotUID, ThroughID: throughID, Events: 1}, record)
	require.Empty(t, pendingSourceUIDs(ctx, t, store, deviceBinding, db.RelayStreamEvent), "a superseded event is not offered again in the old epoch either")
	require.Equal(t, []string{transient.event.EventUID}, pendingSourceUIDs(ctx, t, store, deviceBinding, db.RelayStreamReceipt), "a receipt is root proof the downstream may still need before it can install; it stays owed")

	// Work the root accepts after installation is still owed in the old
	// epoch until the downstream activates, and its acknowledgement verifies
	// although superseded rows below it were never emitted.
	afterInstall := root.created("Accepted after installation", 303, 4)
	root.deliverIssue(ctx, store, afterInstall, 4, 1)
	items, err := store.PendingRelayDeliveries(ctx, deviceBinding, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Len(t, items, 1, "only the post-installation event delivery is owed")
	require.Equal(t, afterInstall.event.EventUID, items[0].SourceUID)
	require.NoError(t, store.AckRelayDeliveries(ctx, deviceBinding, 2, db.RelayStreamEvent, items[0].Sequence, items[0].Digest))
	require.Equal(t, []string{transient.event.EventUID, afterInstall.event.EventUID}, pendingSourceUIDs(ctx, t, store, deviceBinding, db.RelayStreamReceipt))
	forwarded, err = installer.CreateRelayReset(ctx, deviceBinding, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.Equal(t, manifest, forwarded.Manifest)
	require.Equal(t, int64(3), forwarded.Translation.Authority.Epoch)
	deviceEpoch3 := db.WithRelayRequestedEpoch(ctx, 3)
	require.Empty(t, pendingSourceUIDs(deviceEpoch3, t, store, deviceBinding, db.RelayStreamEvent), "a superseded event is never reoffered in the new epoch")
	require.Equal(t, []string{transient.event.EventUID, afterInstall.event.EventUID}, pendingSourceUIDs(deviceEpoch3, t, store, deviceBinding, db.RelayStreamReceipt), "unacknowledged receipts follow the grant into the new epoch")
	_, found = supersededRecord(t, store, project.UID, deviceBinding)
	require.False(t, found, "activation consumes the superseded record")

	// A second downstream pushes its own issue. Until the root accepts it, the
	// first downstream holds a delivery no checkpoint can stand in for.
	siblingUID := "00000000000000000000000009"
	sibling, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: siblingUID, ProtocolVersion: db.RelayProtocolVersion, Token: "forward-downstream-sibling-test-token"})
	require.NoError(t, err)
	siblingBinding := sibling.Enrollment.RelayBindingUID
	siblingIssueUID, err := uid.New()
	require.NoError(t, err)
	siblingEvent := newRemoteEvent(t, project, &siblingIssueUID, "issue.created", "sibling-agent", siblingUID, 303, jsontext.Value(`{"uid":"`+siblingIssueUID+`","title":"Sibling ingress","author":"sibling-agent","metadata":{}}`))
	siblingBody, err := db.EncodeRelaySourceEvent(siblingEvent)
	require.NoError(t, err)
	pushed, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: siblingBinding, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: siblingUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: sibling.Enrollment.RelayResetEpoch, Sequence: 1, Stream: db.RelayStreamEvent, Path: []string{siblingUID}, SourceUID: siblingEvent.EventUID, SourceHash: siblingEvent.ContentHash, Body: siblingBody})
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(ctx, siblingBinding, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{pushed}})
	require.NoError(t, err)
	drainRelayGrant(t, store, upstream, 4)
	require.Contains(t, pendingSourceUIDs(ctx, t, store, deviceBinding, db.RelayStreamEvent), siblingEvent.EventUID)
	manifest, snapshot, translation = root.checkpoint(5, 5, kept)
	err = installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "a downstream owed work the root never accepted still blocks, as its own forward would be refused")
	require.ErrorContains(t, err, deviceBinding)
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, int64(4), binding.RelayConfig.ResetEpoch, "a refused installation changes nothing")

	// Root acceptance arrives; the receipt is newer than the checkpoint, so
	// the retained issue and its proof reach both downstreams in their new epochs.
	siblingReceipt := root.receipt(siblingEvent, 5)
	root.deliverReceipt(ctx, store, siblingReceipt, 4, 2)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation))
	proof, err := store.EntityAttribution(ctx, project.UID, "issue", siblingIssueUID)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootReceipt(pin, proof))
	forwarded, err = installer.CreateRelayReset(ctx, deviceBinding, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.Equal(t, int64(4), forwarded.Translation.Authority.Epoch)
	deviceEpoch4 := db.WithRelayRequestedEpoch(ctx, 4)
	require.Equal(t, []string{siblingEvent.EventUID}, pendingSourceUIDs(deviceEpoch4, t, store, deviceBinding, db.RelayStreamEvent), "root-accepted work newer than the checkpoint follows it into the new epoch")
	require.Contains(t, pendingSourceUIDs(deviceEpoch4, t, store, deviceBinding, db.RelayStreamReceipt), siblingEvent.EventUID)
	forwarded, err = installer.CreateRelayReset(ctx, siblingBinding, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.Equal(t, sibling.Enrollment.RelayResetEpoch+1, forwarded.Translation.Authority.Epoch)
	siblingNext := db.WithRelayRequestedEpoch(ctx, forwarded.Translation.Authority.Epoch)
	require.Empty(t, pendingSourceUIDs(siblingNext, t, store, siblingBinding, db.RelayStreamEvent), "an origin never receives its own event back")
	require.Contains(t, pendingSourceUIDs(siblingNext, t, store, siblingBinding, db.RelayStreamReceipt), siblingEvent.EventUID, "the origin still receives its root proof")
}
