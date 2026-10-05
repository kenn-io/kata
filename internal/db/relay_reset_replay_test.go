package db_test

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R5/A5: corrupt owner backup checkpoints must fail validation before either
// backend clears its target. Restored author labels cannot stand in for a proof.
func TestRelayResetReplayVerifiesRootCheckpoint(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	project := &db.ProjectExport{ID: 42, UID: "00000000000000000000000001", Name: "shared-project"}
	pin := &db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: "00000000000000000000000002", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	provenance, err := json.Marshal(db.RootResetProvenance{Keys: []db.RootKeyPin{*pin}, Receipts: []db.AttributionReceipt{}, Entities: []db.EntityProvenance{}})
	require.NoError(t, err)
	snapshot := db.RootResetSnapshot{Events: []byte(`[]`), Entities: []byte(`[]`), Provenance: provenance, Artifacts: []byte(`[]`)}
	manifest, err := db.SignRootResetManifest(db.RootResetManifest{Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, ResetEpoch: 3, SnapshotUID: "00000000000000000000000003"}, snapshot, private)
	require.NoError(t, err)
	checkpoint := db.RelayResetCheckpoint{Manifest: manifest, Snapshot: snapshot, Translation: db.RelayResetTranslation{Authority: db.RelayHopAuthority{BindingUID: "00000000000000000000000004", ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: pin.AuthorityUID, ReceiverInstanceUID: "00000000000000000000000005", Epoch: 19}, SnapshotUID: manifest.SnapshotUID, SnapshotDigest: manifest.SnapshotDigest}}
	for _, name := range []string{"valid", "valid-served", "signature", "snapshot", "translation", "foreign-key", "no-pin"} {
		t.Run(name, func(t *testing.T) {
			candidate := checkpoint
			key := db.RelayResetMetadataPrefix + project.UID
			if name == "valid-served" {
				key += "." + checkpoint.Translation.Authority.BindingUID
			}
			switch name {
			case "signature":
				candidate.Manifest.Signature = append([]byte(nil), manifest.Signature...)
				candidate.Manifest.Signature[0] ^= 1
			case "snapshot":
				candidate.Snapshot.Events = []byte(`["forged-source"]`)
			case "translation":
				candidate.Translation.SnapshotUID = "00000000000000000000000006"
			case "foreign-key":
				key = db.RelayResetMetadataPrefix + "00000000000000000000000007"
			}
			raw, err := json.Marshal(candidate)
			require.NoError(t, err)
			records := []db.ImportRecord{project, pin, &db.MetaKV{Key: key, Value: string(raw)}}
			if name == "no-pin" {
				records = []db.ImportRecord{project, &db.MetaKV{Key: key, Value: string(raw)}}
			}
			err = db.ValidateImportReplay(records, db.ImportOptions{})
			if name == "valid" || name == "valid-served" {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "invalid checkpoint must fail before target clearing")
			}
		})
	}
}
