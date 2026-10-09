package dbtest

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayRevokedOutboxLifecycle exercises destructive relay lifecycle changes
// with retained deliveries for a revoked descendant. Revocation disables future
// delivery but keeps its outbox rows as audit history.
func RunRelayRevokedOutboxLifecycle(t *testing.T, store db.Storage) {
	for _, action := range []string{"detach", "archive", "reset"} {
		t.Run(action, func(t *testing.T) {
			ctx := t.Context()
			project, config, pin, private, child, source := prepareRevokedRelayOutbox(t, store, action)
			outbox := revokedRelayOutbox(t, store, child.Enrollment.RelayBindingUID)
			require.NotEmpty(t, outbox)
			require.Equal(t, source.EventUID, outbox[0].SourceUID)
			require.False(t, outbox[0].Acknowledged)
			validator, ok := store.(db.RelayLifecycleStore)
			require.True(t, ok)
			require.ErrorIs(t, validator.ValidateRelayLifecycle(ctx, project.ID), db.ErrFederationResetBlockedByPendingPush)

			revoker, ok := store.(db.RelaySelfRevoker)
			require.True(t, ok)
			require.NoError(t, revoker.RevokeOwnRelayEnrollment(ctx, child.Token, project.ID, child.Enrollment.SpokeInstanceUID))

			switch action {
			case "detach":
				_, err := store.LeaveFederationReplica(ctx, project.ID)
				require.NoError(t, err)
			case "archive":
				_, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "local-member", Force: true})
				require.NoError(t, err)
			case "reset":
				require.NoError(t, validator.ValidateRelayLifecycle(ctx, project.ID))
				installer, ok := store.(db.RelayResetStore)
				require.True(t, ok)
				snapshot := emptyRelayResetSnapshot(t, pin)
				snapshotUID, err := uid.New()
				require.NoError(t, err)
				manifest, err := db.SignRootResetManifest(db.RootResetManifest{
					Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID,
					KeyID: pin.KeyID, ResetEpoch: 2, SnapshotUID: snapshotUID,
				}, snapshot, private)
				require.NoError(t, err)
				translation := db.RelayResetTranslation{
					Authority: db.RelayHopAuthority{
						BindingUID: config.BindingUID, ProjectUID: project.UID,
						AuthorityUID: pin.AuthorityUID, SenderInstanceUID: pin.AuthorityUID,
						ReceiverInstanceUID: store.InstanceUID(), Epoch: 2,
					},
					SnapshotUID: snapshotUID, SnapshotDigest: manifest.SnapshotDigest,
				}
				require.NoError(t, installer.InstallRelayReset(ctx, config.BindingUID, manifest, snapshot, translation))

				// A later local write remains real upstream work and must continue to
				// block lifecycle changes and reset installation.
				_, upstreamEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Pending upstream delivery", Author: "local-member"})
				require.NoError(t, err)
				upstreamPending, err := store.PendingRelayDeliveries(ctx, config.BindingUID, db.RelayStreamEvent, 10)
				require.NoError(t, err)
				require.Len(t, upstreamPending, 1)
				require.Equal(t, upstreamEvent.UID, upstreamPending[0].SourceUID)
				require.ErrorIs(t, validator.ValidateRelayLifecycle(ctx, project.ID), db.ErrFederationResetBlockedByPendingPush)
				nextUID, err := uid.New()
				require.NoError(t, err)
				nextManifest, err := db.SignRootResetManifest(db.RootResetManifest{
					Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID,
					KeyID: pin.KeyID, ResetEpoch: 3, SnapshotUID: nextUID,
				}, snapshot, private)
				require.NoError(t, err)
				nextTranslation := translation
				nextTranslation.Authority.Epoch = 3
				nextTranslation.SnapshotUID = nextUID
				nextTranslation.SnapshotDigest = nextManifest.SnapshotDigest
				require.ErrorIs(t, installer.InstallRelayReset(ctx, config.BindingUID, nextManifest, snapshot, nextTranslation), db.ErrFederationResetBlockedByPendingPush)
			}

			retained := revokedRelayOutbox(t, store, child.Enrollment.RelayBindingUID)
			require.Len(t, retained, len(outbox))
			require.Equal(t, outbox[0], retained[0], "revocation and lifecycle changes retain exact downstream delivery history")
		})
	}
}

func prepareRevokedRelayOutbox(t *testing.T, store db.Storage, suffix string) (db.Project, db.RelayBindingConfig, db.RootKeyPin, ed25519.PrivateKey, db.CreatedFederationEnrollment, db.RemoteEvent) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "relay-project-"+suffix)
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID,
		Actor: "company-member", PushEnabled: true, Enabled: true,
	})
	require.NoError(t, err)
	bindingUID, err := uid.New()
	require.NoError(t, err)
	config := db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID,
		UpstreamInstanceUID: rootUID, AuthorityUID: rootUID,
		HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "local-member",
		ServeDownstream: true, ResetEpoch: 1,
	}
	_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
		Actor: "local-member", AdminActor: "admin", PlaintextToken: "revoked-outbox-parent-" + suffix,
	})
	require.NoError(t, err)
	peer := "00000000000000000000000006"
	child, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peer,
		ProtocolVersion: db.RelayProtocolVersion, Token: "revoked-outbox-child-" + suffix,
	})
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	source := newRemoteEvent(t, project, &issueUID, "issue.created", "source-assistant", rootUID, 300,
		jsontext.Value(`{"uid":"`+issueUID+`","title":"Incoming source","author":"source-assistant","metadata":{}}`))
	body, err := db.EncodeRelaySourceEvent(source)
	require.NoError(t, err)
	envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{
		Version: db.RelayProtocolVersion, BindingUID: config.BindingUID,
		ProjectUID: project.UID, AuthorityUID: rootUID,
		SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(),
		Epoch: config.ResetEpoch, Sequence: 1, Stream: db.RelayStreamEvent,
		Path: []string{rootUID}, SourceUID: source.EventUID, SourceHash: source.ContentHash,
		Body: body,
	})
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(ctx, config.BindingUID, db.RelayBatch{
		Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope},
	})
	require.NoError(t, err)
	return project, config, pin, private, child, source
}

func emptyRelayResetSnapshot(t *testing.T, pin db.RootKeyPin) db.RootResetSnapshot {
	t.Helper()
	provenance, err := json.Marshal(db.RootResetProvenance{
		Keys: []db.RootKeyPin{pin}, Receipts: []db.AttributionReceipt{}, Entities: []db.EntityProvenance{},
	})
	require.NoError(t, err)
	return db.RootResetSnapshot{Events: []byte(`[]`), Entities: []byte(`[]`), Provenance: provenance, Artifacts: []byte(`[]`)}
}

func revokedRelayOutbox(t *testing.T, store db.Storage, bindingUID string) []*db.RelayOutboxExport {
	t.Helper()
	records := []*db.RelayOutboxExport{}
	for record, err := range store.ExportRelayState(t.Context()) {
		require.NoError(t, err)
		outbox, ok := record.(*db.RelayOutboxExport)
		if ok && outbox.BindingUID == bindingUID {
			records = append(records, outbox)
		}
	}
	return records
}
