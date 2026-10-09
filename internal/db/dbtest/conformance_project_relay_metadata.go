package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunProjectPurgeRemovesRelayMetadata verifies purge removes relay metadata.
func RunProjectPurgeRemovesRelayMetadata(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	ctx := t.Context()
	project := detachedResetProject(t, store)
	_, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "example-actor", Force: true,
	})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{
		ProjectID: project.ID, Actor: "example-actor",
	})
	require.NoError(t, err)
	requirePortableBackupReplay(t, store, backend)
}

// RunProjectMergeRemovesRelayMetadata verifies merge removes source relay metadata.
func RunProjectMergeRemovesRelayMetadata(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	ctx := t.Context()
	source := detachedResetProject(t, store)
	target, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: source.ID, TargetProjectID: target.ID, Actor: "example-actor",
	})
	require.NoError(t, err)
	requirePortableBackupReplay(t, store, backend)
}

// RunArchivedDetachedRelayMetadataRetainsRootKeys verifies archival keeps root keys.
func RunArchivedDetachedRelayMetadataRetainsRootKeys(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project := detachedResetProject(t, store)
	attribution, ok := store.(db.AttributionStorage)
	require.True(t, ok)
	want := make(map[string]db.RootKeyPin)
	for record, err := range attribution.ExportAttribution(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		if pin, ok := record.(*db.RootKeyPin); ok && pin.ProjectUID == project.UID {
			want[pin.KeyID] = *pin
		}
	}
	require.Len(t, want, 2, "the signed reset and rotation retain both verification keys")
	_, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "example-actor", Force: true,
	})
	require.NoError(t, err)
	got := make(map[string]db.RootKeyPin)
	for record, err := range attribution.ExportAttribution(ctx, db.ExportFilter{IncludeDeleted: false}) {
		require.NoError(t, err)
		if pin, ok := record.(*db.RootKeyPin); ok && pin.ProjectUID == project.UID {
			got[pin.KeyID] = *pin
		}
	}
	require.Equal(t, want, got,
		"live-only backups must include every retained verification key referenced by archived signed relay metadata")
}

func detachedResetProject(t *testing.T, store db.Storage) db.Project {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: rootUID,
		KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
	}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: project.UID, Actor: "example-actor", Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)
	bindingUID, err := uid.New()
	require.NoError(t, err)
	config := db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID,
		UpstreamInstanceUID: rootUID, AuthorityUID: rootUID,
		HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "example-actor",
		ServeDownstream: true, ResetEpoch: 1,
	}
	_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
	require.NoError(t, err)
	provenance, err := json.Marshal(db.RootResetProvenance{
		Keys: []db.RootKeyPin{pin}, Receipts: []db.AttributionReceipt{}, Entities: []db.EntityProvenance{},
	})
	require.NoError(t, err)
	snapshot := db.RootResetSnapshot{
		Events: []byte("[]"), Entities: []byte("[]"),
		Provenance: provenance, Artifacts: []byte("[]"),
	}
	snapshotUID, err := uid.New()
	require.NoError(t, err)
	manifest, err := db.SignRootResetManifest(db.RootResetManifest{
		Version: 1, ProjectUID: project.UID, AuthorityUID: rootUID,
		KeyID: pin.KeyID, ResetEpoch: 1, SnapshotUID: snapshotUID,
	}, snapshot, privateKey)
	require.NoError(t, err)
	translation := db.RelayResetTranslation{
		Authority: db.RelayHopAuthority{
			BindingUID: bindingUID, ProjectUID: project.UID, AuthorityUID: rootUID,
			SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: 2,
		},
		SnapshotUID: snapshotUID, SnapshotDigest: manifest.SnapshotDigest,
	}
	installer, ok := store.(interface {
		InstallRelayReset(context.Context, string, db.RootResetManifest, db.RootResetSnapshot, db.RelayResetTranslation) error
	})
	require.True(t, ok)
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, translation))
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	nextPublic, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	nextPin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: rootUID,
		KeyID: db.RootPublicKeyID(nextPublic), PublicKey: nextPublic,
	}
	transition, err := db.SignRootKeyTransition(pin, nextPin, privateKey)
	require.NoError(t, err)
	require.NoError(t, store.RotateRootAuthority(ctx, transition))
	return project
}

func requirePortableBackupReplay(t *testing.T, source db.Storage, backend Backend) {
	t.Helper()
	ctx := t.Context()
	records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	for record, err := range source.ExportAttribution(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		records = append(records, record)
	}
	target := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportReplay(ctx, records, db.ImportOptions{}),
		"full backup replay must not retain project-owned relay checkpoints or root transitions after project deletion")
}
