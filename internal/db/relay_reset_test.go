package db_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R4 binds all four snapshot sections and named ROOT baselines. Immediate hop
// translations authenticate independently and need not equal root sequences.
func TestSignedRelayResetCommitments(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: "00000000000000000000000002", AuthorityUID: "00000000000000000000000003", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	snapshot := db.RootResetSnapshot{Events: []byte("events\n"), Entities: []byte("entities\n"), Provenance: []byte("receipts and creators\n"), Artifacts: []byte("complete artifact manifests\n")}
	manifest := db.RootResetManifest{Version: 1, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, ResetEpoch: 3, SnapshotUID: "00000000000000000000000007", RootBaselines: db.RelayStreamCursors{Events: 101, Receipts: 33, Artifacts: 7}}
	signed, err := db.SignRootResetManifest(manifest, snapshot, private)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootResetManifest(pin, signed, snapshot))
	require.Empty(t, manifest.Signature)
	require.Empty(t, manifest.SnapshotDigest)
	for _, change := range []func(*db.RootResetSnapshot){
		func(s *db.RootResetSnapshot) { s.Events = append(bytes.Clone(s.Events), 'x') },
		func(s *db.RootResetSnapshot) { s.Entities = append(bytes.Clone(s.Entities), 'x') },
		func(s *db.RootResetSnapshot) { s.Provenance = append(bytes.Clone(s.Provenance), 'x') },
		func(s *db.RootResetSnapshot) { s.Artifacts = append(bytes.Clone(s.Artifacts), 'x') },
	} {
		bad := snapshot
		change(&bad)
		require.Error(t, db.VerifyRootResetManifest(pin, signed, bad))
	}
	changed := signed
	changed.RootBaselines.Receipts++
	require.Error(t, db.VerifyRootResetManifest(pin, changed, snapshot))
	changed = signed
	changed.SnapshotUID = "00000000000000000000000008"
	require.Error(t, db.VerifyRootResetManifest(pin, changed, snapshot))
	badPin := pin
	badPin.AuthorityUID = "00000000000000000000000009"
	require.Error(t, db.VerifyRootResetManifest(badPin, signed, snapshot))
	grant := db.RelayHopAuthority{BindingUID: "00000000000000000000000001", ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: "00000000000000000000000004", ReceiverInstanceUID: "00000000000000000000000005", Epoch: 3}
	translation := db.RelayResetTranslation{Authority: grant, SnapshotUID: signed.SnapshotUID, SnapshotDigest: signed.SnapshotDigest, HopBaselines: db.RelayStreamCursors{Events: 9, Receipts: 5, Artifacts: 2}}
	require.NoError(t, db.ValidateRelayResetTranslation(grant, signed, translation))
	require.Equal(t, int64(101), signed.RootBaselines.Events)
	// A hop can have been rebound/reset independently of the root's checkpoint.
	// Its authenticated epoch and translated cursors never rewrite root proof.
	grant.Epoch = 19
	translation.Authority = grant
	require.NoError(t, db.ValidateRelayResetTranslation(grant, signed, translation), "binding-local epoch must be independent of signed root epoch")
	require.Equal(t, int64(3), signed.ResetEpoch)
	require.NoError(t, db.VerifyRootResetManifest(pin, signed, snapshot))
	for _, invalidEpoch := range []int64{0, -1} {
		invalid := signed
		invalid.ResetEpoch = invalidEpoch
		require.Error(t, db.ValidateRelayResetTranslation(grant, invalid, translation))
	}
	translation.Authority.BindingUID = "00000000000000000000000009"
	require.Error(t, db.ValidateRelayResetTranslation(grant, signed, translation))
	translation.Authority = grant
	translation.SnapshotUID = "00000000000000000000000009"
	require.Error(t, db.ValidateRelayResetTranslation(grant, signed, translation))
	translation.SnapshotUID = signed.SnapshotUID
	translation.Authority.Epoch--
	require.Error(t, db.ValidateRelayResetTranslation(grant, signed, translation))
	snapshot.Artifacts = nil
	_, err = db.SignRootResetManifest(manifest, snapshot, private)
	require.Error(t, err, "cannot omit a committed section")
}

func FuzzRootResetExactCommitment(f *testing.F) {
	f.Add([]byte("root key seed"), []byte("snapshot section"), uint8(0), uint64(17))
	f.Add([]byte{}, []byte{}, uint8(3), uint64(0))
	f.Fuzz(func(t *testing.T, seed, section []byte, kind uint8, sequence uint64) {
		material := sha256.Sum256(seed)
		private := ed25519.NewKeyFromSeed(material[:])
		public := private.Public().(ed25519.PublicKey)
		pin := db.RootKeyPin{ProjectUID: "00000000000000000000000002", AuthorityUID: "00000000000000000000000003", KeyID: db.RootPublicKeyID(public), PublicKey: public}
		snapshot := db.RootResetSnapshot{Events: []byte{}, Entities: []byte{}, Provenance: []byte{}, Artifacts: []byte{}}
		sections := []*[]byte{&snapshot.Events, &snapshot.Entities, &snapshot.Provenance, &snapshot.Artifacts}
		chosen := sections[int(kind)%len(sections)]
		*chosen = append([]byte{}, section...)
		manifest := db.RootResetManifest{Version: 1, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, ResetEpoch: 1, SnapshotUID: "00000000000000000000000007", RootBaselines: db.RelayStreamCursors{Events: int64(sequence & math.MaxInt64)}}
		signed, err := db.SignRootResetManifest(manifest, snapshot, private)
		require.NoError(t, err)
		require.NoError(t, db.VerifyRootResetManifest(pin, signed, snapshot))
		*chosen = append(bytes.Clone(*chosen), 0)
		require.Error(t, db.VerifyRootResetManifest(pin, signed, snapshot), "changing any complete section invalidates its checkpoint")
	})
}
