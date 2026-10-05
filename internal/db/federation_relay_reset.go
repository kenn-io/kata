package db

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/uid"
)

// RootResetSnapshot commits every portable snapshot section. Empty sections
// must be explicit; omission is not an empty authoritative manifest.
type RootResetSnapshot struct {
	Events     []byte `json:"events"`
	Entities   []byte `json:"entities"`
	Provenance []byte `json:"provenance"`
	Artifacts  []byte `json:"artifacts"`
}

// RootResetManifest signs a complete snapshot, root stream baselines and reset epoch.
type RootResetManifest struct {
	Version        int                `json:"version"`
	ProjectUID     string             `json:"project_uid"`
	AuthorityUID   string             `json:"authority_uid"`
	KeyID          string             `json:"key_id"`
	ResetEpoch     int64              `json:"reset_epoch"`
	SnapshotUID    string             `json:"snapshot_uid"`
	SnapshotDigest string             `json:"snapshot_digest"`
	RootBaselines  RelayStreamCursors `json:"root_baselines"`
	// HistoryEventID is the root-local replay boundary captured by this snapshot.
	// It detects subsequent purge gaps independently of hop/outbox sequences.
	HistoryEventID int64  `json:"history_event_id,omitzero"`
	Signature      []byte `json:"signature,omitzero"`
}

// RelayResetTranslation is immediate-hop authority, not a re-signed root
// checkpoint. It is installed with that exact checkpoint and snapshot.
type RelayResetTranslation struct {
	Authority      RelayHopAuthority  `json:"authority"`
	SnapshotUID    string             `json:"snapshot_uid"`
	SnapshotDigest string             `json:"snapshot_digest"`
	HopBaselines   RelayStreamCursors `json:"hop_baselines"`
}

// RootResetSnapshotDigest commits every explicit portable snapshot section.
func RootResetSnapshotDigest(snapshot RootResetSnapshot) (string, error) {
	sections := []struct {
		name string
		data []byte
	}{{"events", snapshot.Events}, {"entities", snapshot.Entities}, {"provenance", snapshot.Provenance}, {"artifacts", snapshot.Artifacts}}
	h := sha256.New()
	_, _ = h.Write([]byte("kata.root-reset-snapshot.v1\x00"))
	for _, section := range sections {
		if section.data == nil {
			return "", errors.New("root reset snapshot omits a committed section")
		}
		_, _ = h.Write([]byte(section.name + "\x00"))
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(section.data)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(section.data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func rootResetSigningBytes(manifest RootResetManifest) ([]byte, error) {
	if manifest.Version != 1 || !uid.Valid(manifest.ProjectUID) || !uid.Valid(manifest.AuthorityUID) || !uid.Valid(manifest.SnapshotUID) || manifest.ResetEpoch <= 0 || manifest.HistoryEventID < 0 || !manifest.RootBaselines.valid() || !validRelayDigest(manifest.KeyID) || !validRelayDigest(manifest.SnapshotDigest) {
		return nil, errors.New("invalid root reset manifest")
	}
	manifest.Signature = nil
	return json.Marshal(struct {
		Domain   string            `json:"domain"`
		Manifest RootResetManifest `json:"manifest"`
	}{"kata.root-reset.v1", manifest})
}

// SignRootResetManifest signs the manifest with its pinned root authority.
func SignRootResetManifest(manifest RootResetManifest, snapshot RootResetSnapshot, private ed25519.PrivateKey) (RootResetManifest, error) {
	if len(private) != ed25519.PrivateKeySize || manifest.KeyID != RootPublicKeyID(private.Public().(ed25519.PublicKey)) {
		return RootResetManifest{}, errors.New("root reset signing key does not match pinned key")
	}
	digest, err := RootResetSnapshotDigest(snapshot)
	if err != nil {
		return RootResetManifest{}, err
	}
	manifest.SnapshotDigest = digest
	canonical, err := rootResetSigningBytes(manifest)
	if err != nil {
		return RootResetManifest{}, err
	}
	manifest.Signature = ed25519.Sign(private, canonical)
	return manifest, nil
}

// VerifyRootResetManifest verifies the pinned signature and complete snapshot digest.
func VerifyRootResetManifest(pin RootKeyPin, manifest RootResetManifest, snapshot RootResetSnapshot) error {
	if err := ValidateRootKeyPin(pin); err != nil {
		return err
	}
	if pin.ProjectUID != manifest.ProjectUID || pin.AuthorityUID != manifest.AuthorityUID || pin.KeyID != manifest.KeyID || len(manifest.Signature) != ed25519.SignatureSize {
		return errors.New("root reset does not match pinned authority")
	}
	canonical, err := rootResetSigningBytes(manifest)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(pin.PublicKey), canonical, manifest.Signature) {
		return errors.New("invalid root reset signature")
	}
	digest, err := RootResetSnapshotDigest(snapshot)
	if err != nil {
		return err
	}
	if digest != manifest.SnapshotDigest {
		return errors.New("root reset snapshot commitment mismatch")
	}
	return nil
}

// ValidateRelayResetTranslation checks the checkpoint against the authenticated hop.
func ValidateRelayResetTranslation(authority RelayHopAuthority, manifest RootResetManifest, translation RelayResetTranslation) error {
	if err := validateRelayHop(authority); err != nil {
		return err
	}
	if translation.Authority != authority || manifest.ProjectUID != authority.ProjectUID || manifest.AuthorityUID != authority.AuthorityUID || manifest.ResetEpoch <= 0 || translation.SnapshotUID != manifest.SnapshotUID || translation.SnapshotDigest != manifest.SnapshotDigest || !translation.HopBaselines.valid() {
		return errors.New("relay reset translation does not match authenticated hop and root checkpoint")
	}
	return nil
}

// RelayResetStore is the native atomic checkpoint capability used by the
// existing federation transport and runner.
type RelayResetStore interface {
	CreateRelayReset(context.Context, string, RootAttributionSigner) (RelayResetCheckpoint, error)
	InstallRelayReset(context.Context, string, RootResetManifest, RootResetSnapshot, RelayResetTranslation) error
}

// RelayResetBootstrapStore detects history that requires a signed snapshot.
type RelayResetBootstrapStore interface {
	RelayEnrollmentNeedsReset(context.Context, string) (bool, error)
}

// WithRelayRequestedEpoch carries the peer's requested namespace, never authority.
// Native storage validates it against the live grant and a retained checkpoint.
func WithRelayRequestedEpoch(ctx context.Context, epoch int64) context.Context {
	return context.WithValue(ctx, relayRequestedEpochKey{}, epoch)
}

type relayRequestedEpochKey struct{}

// RelayRequestedEpoch returns the requested namespace, never an authorization grant.
func RelayRequestedEpoch(ctx context.Context) int64 {
	epoch, _ := ctx.Value(relayRequestedEpochKey{}).(int64)
	return epoch
}
