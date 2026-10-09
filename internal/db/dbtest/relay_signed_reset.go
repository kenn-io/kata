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

// RunSignedRelayResetCommitments exercises signed relay reset commitments on the supplied native store.
// R4/R5: a root-signed checkpoint and the live immediate-hop translation
// replace state together. Source labels alone never confer verified creation.
func RunSignedRelayResetCommitments(t *testing.T, store db.Storage) {
	installer, ok := store.(interface {
		InstallRelayReset(context.Context, string, db.RootResetManifest, db.RootResetSnapshot, db.RelayResetTranslation) error
	})
	require.True(t, ok, "native atomic signed reset installation is required")
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "reset-replica")
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	bindingUID, err := uid.New()
	require.NoError(t, err)
	config := db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, AuthorityUID: rootUID, UpstreamInstanceUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "personal-member", ServeDownstream: true, ResetEpoch: 1}
	_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	source := newRemoteEvent(t, project, &issueUID, "issue.created", "source-assistant", rootUID, 300, jsontext.Value(`{"uid":"`+issueUID+`","title":"Before snapshot","author":"source-assistant","metadata":{}}`))
	sourceBytes, err := db.EncodeRelaySourceEvent(source)
	require.NoError(t, err)
	proof, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: project.UID, EventUID: source.EventUID, ContentHash: source.ContentHash, AuthorityUID: rootUID, KeyID: pin.KeyID, AccountableActor: "company-member", SourceActor: source.Actor, IngressInstanceUID: rootUID, AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: 1}, private)
	require.NoError(t, err)
	// Snapshots describe current state but are not original creation events.
	entity := newRemoteEvent(t, project, &issueUID, "issue.snapshot", "source-assistant", rootUID, 400, jsontext.Value(`{"uid":"`+issueUID+`","title":"After snapshot","author":"source-assistant","metadata":{},"comments":[],"labels":[],"links":[]}`))
	entityBytes, err := db.EncodeRelaySourceEvent(entity)
	require.NoError(t, err)
	events, err := json.Marshal([][]byte{sourceBytes})
	require.NoError(t, err)
	legacyUID, err := uid.New()
	require.NoError(t, err)
	legacy := newRemoteEvent(t, project, &legacyUID, "issue.snapshot", "historical-author", rootUID, 400, jsontext.Value(`{"uid":"`+legacyUID+`","title":"Legacy snapshot","author":"historical-author","metadata":{},"comments":[],"labels":[],"links":[]}`))
	legacyBytes, err := db.EncodeRelaySourceEvent(legacy)
	require.NoError(t, err)
	entities, err := json.Marshal([][]byte{entityBytes, legacyBytes})
	require.NoError(t, err)
	provenance, err := json.Marshal(struct {
		Keys     []db.RootKeyPin         `json:"keys"`
		Receipts []db.AttributionReceipt `json:"receipts"`
		Entities []db.EntityProvenance   `json:"entities"`
	}{[]db.RootKeyPin{pin}, []db.AttributionReceipt{proof}, []db.EntityProvenance{{ProjectUID: project.UID, Kind: "issue", EntityUID: issueUID, EventUID: source.EventUID}}})
	require.NoError(t, err)
	snapshot := db.RootResetSnapshot{Events: events, Entities: entities, Provenance: provenance, Artifacts: []byte(`[]`)}
	snapshotUID, err := uid.New()
	require.NoError(t, err)
	manifest, err := db.SignRootResetManifest(db.RootResetManifest{Version: 1, ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: pin.KeyID, ResetEpoch: 3, SnapshotUID: snapshotUID, RootBaselines: db.RelayStreamCursors{Events: 11, Receipts: 13, Artifacts: 17}}, snapshot, private)
	require.NoError(t, err)
	translation := db.RelayResetTranslation{Authority: db.RelayHopAuthority{BindingUID: bindingUID, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: 19}, SnapshotUID: snapshotUID, SnapshotDigest: manifest.SnapshotDigest, HopBaselines: db.RelayStreamCursors{Events: 101, Receipts: 113, Artifacts: 117}}
	for _, name := range []string{"signature", "snapshot", "hop", "hidden"} {
		t.Run(name, func(t *testing.T) {
			badManifest, badSnapshot, badTranslation := manifest, snapshot, translation
			testCtx := ctx
			switch name {
			case "signature":
				badManifest.Signature = append([]byte(nil), manifest.Signature...)
				badManifest.Signature[0] ^= 1
			case "snapshot":
				badSnapshot.Entities = []byte(`[]`)
			case "hop":
				badTranslation.Authority.SenderInstanceUID = store.InstanceUID()
			case "hidden":
				testCtx = db.WithAuthorizedProjects(ctx, []string{})
			}
			require.Error(t, installer.InstallRelayReset(testCtx, bindingUID, badManifest, badSnapshot, badTranslation))
			retained, err := store.FederationBindingByProject(ctx, project.ID)
			require.NoError(t, err)
			require.Equal(t, int64(1), retained.RelayConfig.ResetEpoch)
		})
	}
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "reset-parent-test-token", Actor: "personal-member", AdminActor: "admin"})
	require.NoError(t, err)
	child, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion, Token: "reset-child-test-token"})
	require.NoError(t, err)
	require.ErrorContains(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, translation), "active downstream enrollments")
	require.NoError(t, store.RevokeFederationEnrollment(ctx, child.Enrollment.ID))
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, translation))
	issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "After snapshot", issue.Title)
	require.Equal(t, "verified", issue.Verification)
	require.Equal(t, "company-member", issue.AccountableActor)
	require.Equal(t, "source-assistant", issue.Author)
	legacyIssue, err := store.IssueByUID(ctx, legacyUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "legacy", legacyIssue.Verification, "root-signed state does not promote a snapshot author into verified creation")
	require.Empty(t, legacyIssue.AccountableActor)
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, int64(19), binding.RelayConfig.ResetEpoch)
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, translation), "lost commit response retries the exact retained checkpoint")
	cursors := map[string]int64{}
	for record, err := range store.ExportRelayState(ctx) {
		require.NoError(t, err)
		if cursor, ok := record.(*db.RelayCursorExport); ok && cursor.BindingUID == bindingUID && cursor.ResetEpoch == 19 {
			cursors[cursor.Stream] = cursor.AcceptedThrough
			require.Equal(t, cursor.AcceptedThrough, cursor.OfferedThrough)
			require.Zero(t, cursor.AcknowledgedThrough)
		}
	}
	require.Equal(t, map[string]int64{db.RelayStreamEvent: 101, db.RelayStreamReceipt: 113, db.RelayStreamArtifact: 117}, cursors, "install hop baselines, never root sequence numbers")
	received, err := store.EventsByUIDs(ctx, project.ID, []string{source.EventUID})
	require.NoError(t, err)
	require.Len(t, received, 1)
	require.Equal(t, source, db.RemoteEventFromStored(received[0]))
	emitted, err := store.PendingRelayDeliveries(ctx, bindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Empty(t, emitted, "snapshot installation cannot echo imported source as local intent")

	retranslated := translation
	retranslated.Authority.Epoch = 20
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, retranslated), "a new authenticated hop epoch may map the unchanged root checkpoint")
	// A relay forwards the exact root checkpoint; it authenticates only the
	// child hop translation. An unrelated local signing key grants no authority.
	forwarder, ok := store.(interface {
		CreateRelayReset(context.Context, string, db.RootAttributionSigner) (db.RelayResetCheckpoint, error)
	})
	require.True(t, ok)
	forwardChild, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000009", ProtocolVersion: db.RelayProtocolVersion, Token: "reset-forward-child-test-token"})
	require.NoError(t, err)
	_, err = forwarder.CreateRelayReset(ctx, forwardChild.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "initial emitted child intent must be resolved first")
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt} {
		items, err := store.PendingRelayDeliveries(ctx, forwardChild.Enrollment.RelayBindingUID, stream, 100)
		require.NoError(t, err)
		if len(items) > 0 {
			last := items[len(items)-1]
			require.NoError(t, store.AckRelayDeliveries(ctx, forwardChild.Enrollment.RelayBindingUID, 1, stream, last.Sequence, last.Digest))
		}
	}
	forwarded, err := forwarder.CreateRelayReset(ctx, forwardChild.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	require.NoError(t, err, "relay must forward its retained signed checkpoint without a root private key")
	require.Equal(t, manifest, forwarded.Manifest)
	require.Equal(t, snapshot, forwarded.Snapshot)
	require.Equal(t, db.RelayHopAuthority{BindingUID: forwardChild.Enrollment.RelayBindingUID, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: store.InstanceUID(), ReceiverInstanceUID: forwardChild.Enrollment.SpokeInstanceUID, Epoch: 2}, forwarded.Translation.Authority)
	require.Equal(t, db.RelayStreamCursors{}, forwarded.Translation.HopBaselines)
	retryForward, err := forwarder.CreateRelayReset(ctx, forwardChild.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.Equal(t, forwarded, retryForward, "lost response retries the same child mapping")
	_, err = forwarder.CreateRelayReset(db.WithAuthorizedProjects(ctx, []string{}), forwardChild.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	require.Error(t, err, "cached checkpoint cannot bypass project visibility")
	revokedUpstream := config
	revokedUpstream.ResetEpoch = retranslated.Authority.Epoch
	revokedUpstream.UpstreamRevoked = true
	_, err = store.SetRelayBindingConfig(ctx, project.ID, revokedUpstream)
	require.NoError(t, err)
	_, err = forwarder.CreateRelayReset(ctx, forwardChild.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	require.Error(t, err, "observed upstream revocation blocks cached downstream checkpoint")
	revokedUpstream.UpstreamRevoked = false
	_, err = store.SetRelayBindingConfig(ctx, project.ID, revokedUpstream)
	require.NoError(t, err)
	require.NoError(t, store.RevokeFederationEnrollment(ctx, forwardChild.Enrollment.ID))
	_, err = forwarder.CreateRelayReset(ctx, forwardChild.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	require.Error(t, err, "revoked child cannot download cached checkpoint")
	conflicting := manifest
	conflicting.SnapshotUID, err = uid.New()
	require.NoError(t, err)
	conflicting, err = db.SignRootResetManifest(conflicting, snapshot, private)
	require.NoError(t, err)
	conflictingHop := retranslated
	conflictingHop.Authority.Epoch = 21
	conflictingHop.SnapshotUID = conflicting.SnapshotUID
	conflictingHop.SnapshotDigest = conflicting.SnapshotDigest
	require.Error(t, installer.InstallRelayReset(ctx, bindingUID, conflicting, snapshot, conflictingHop), "equal root epoch cannot select a different checkpoint")
	stale := translation
	stale.Authority.Epoch = 18
	require.Error(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, stale))
	// A collision discovered after clearing the replica rolls back that clear,
	// checkpoint replacement and all cursor changes together.
	foreignProject, err := store.CreateProject(ctx, "reset-sibling")
	require.NoError(t, err)
	foreignIssue, foreignEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: foreignProject.ID, Title: "Keep sibling", Author: "personal-member"})
	require.NoError(t, err)
	collision := newRemoteEvent(t, project, &foreignIssue.UID, "issue.created", "personal-member", rootUID, 500, jsontext.Value(`{"uid":"`+foreignIssue.UID+`","title":"Collision","author":"personal-member","metadata":{}}`))
	collision.EventUID = foreignEvent.UID
	collision.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: collision.EventUID, OriginInstanceUID: collision.OriginInstanceUID, ProjectUID: collision.ProjectUID, ProjectName: collision.ProjectName, IssueUID: collision.IssueUID, Type: collision.Type, Actor: collision.Actor, HLCPhysicalMS: collision.HLCPhysicalMS, HLCCounter: collision.HLCCounter, CreatedAt: collision.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: collision.Payload})
	require.NoError(t, err)
	collisionBytes, err := db.EncodeRelaySourceEvent(collision)
	require.NoError(t, err)
	interruptedSnapshot := snapshot
	interruptedSnapshot.Events, err = json.Marshal([][]byte{sourceBytes, collisionBytes})
	require.NoError(t, err)
	interruptedManifest := manifest
	interruptedManifest.ResetEpoch = 4
	interruptedManifest.SnapshotUID, err = uid.New()
	require.NoError(t, err)
	interruptedManifest, err = db.SignRootResetManifest(interruptedManifest, interruptedSnapshot, private)
	require.NoError(t, err)
	interruptedHop := retranslated
	interruptedHop.Authority.Epoch = 21
	interruptedHop.SnapshotUID = interruptedManifest.SnapshotUID
	interruptedHop.SnapshotDigest = interruptedManifest.SnapshotDigest
	require.Error(t, installer.InstallRelayReset(ctx, bindingUID, interruptedManifest, interruptedSnapshot, interruptedHop))
	restored, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "After snapshot", restored.Title)
	require.Equal(t, "verified", restored.Verification)
	retainedBinding, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, int64(20), retainedBinding.RelayConfig.ResetEpoch)
	sibling, err := store.IssueByUID(ctx, foreignIssue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "Keep sibling", sibling.Title)
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, retranslated), "the retained checkpoint remains retryable after rollback")
	// Newly created offline intent must block the next destructive reset.
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Pending local intent", Author: "personal-member"})
	require.NoError(t, err)
	nextUID, err := uid.New()
	require.NoError(t, err)
	nextManifest := manifest
	nextManifest.SnapshotUID = nextUID
	nextManifest.ResetEpoch = 4
	nextManifest, err = db.SignRootResetManifest(nextManifest, snapshot, private)
	require.NoError(t, err)
	nextTranslation := translation
	nextTranslation.Authority.Epoch = 21
	nextTranslation.SnapshotUID = nextUID
	nextTranslation.SnapshotDigest = nextManifest.SnapshotDigest
	require.ErrorIs(t, installer.InstallRelayReset(ctx, bindingUID, nextManifest, snapshot, nextTranslation), db.ErrFederationResetBlockedByPendingPush)
	offered, err := store.PendingRelayDeliveries(ctx, bindingUID, db.RelayStreamEvent, 1024)
	require.NoError(t, err)
	require.NotEmpty(t, offered)
	last := offered[len(offered)-1]
	require.NoError(t, store.AckRelayDeliveries(ctx, bindingUID, last.Epoch, last.Stream, last.Sequence, last.Digest))
	require.ErrorIs(t, installer.InstallRelayReset(ctx, bindingUID, nextManifest, snapshot, nextTranslation), db.ErrFederationResetBlockedByPendingPush, "a hop ACK cannot clear root-pending reset blockers")
	binding, err = store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, int64(20), binding.RelayConfig.ResetEpoch)
	newPublic, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	nextPin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(newPublic), PublicKey: newPublic}
	transition, err := db.SignRootKeyTransition(pin, nextPin, private)
	require.NoError(t, err)
	require.NoError(t, store.RotateRootAuthority(ctx, transition))
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, retranslated), "pinned historical root signatures remain valid after signed rotation")
	currentPin, err := store.RootAuthority(ctx, project.UID)
	require.NoError(t, err)
	require.Equal(t, nextPin.KeyID, currentPin.KeyID, "historical checkpoint cannot replace the live root pin")
	binding.Enabled = false
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	require.Error(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, translation), "exact retry still requires current transport authority")
}
