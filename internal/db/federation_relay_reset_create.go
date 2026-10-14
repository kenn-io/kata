package db

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/kata/internal/uid"
)

// RelayCheckpointStateEvent is one current-state record checkpoint creation
// writes into the project's history at the capture boundary's clock.
type RelayCheckpointStateEvent struct {
	Project   Project
	Type      string
	Actor     string
	Payload   string
	Clock     EventHLCTimestamp
	CreatedAt time.Time
}

// NextRootResetEpoch returns the root epoch a new checkpoint signs, above
// every checkpoint the root retained for the project. Root epochs are per
// project, independent of every binding's hop epoch, so a second enrollment
// cannot reuse an epoch with a different signed snapshot. Epoch 1 is the
// enrollment baseline; the first checkpoint is epoch 2.
func NextRootResetEpoch(retained []string) (int64, error) {
	rootEpoch := int64(1)
	for _, value := range retained {
		var checkpoint RelayResetCheckpoint
		if err := json.Unmarshal([]byte(value), &checkpoint, json.RejectUnknownMembers(true)); err != nil {
			return 0, err
		}
		rootEpoch = max(rootEpoch, checkpoint.Manifest.ResetEpoch)
	}
	return rootEpoch + 1, nil
}

// RelayResetRetryEpoch decides whether a retained checkpoint may be disclosed
// to the grant again: only one prepared for the grant's current epoch or the
// next one belongs to this enrollment.
func RelayResetRetryEpoch(cached, current int64) error {
	if cached != current && cached != current+1 {
		return ErrFederationIngestValidation
	}
	return nil
}

// relayResetCreate is the enrollment a checkpoint is being created for.
type relayResetCreate struct {
	bindingUID string
	grant      FederationEnrollment
	project    Project
	pin        RootKeyPin
	key        string
}

// CreateReset captures a root's current snapshot and creation proofs under
// its existing native transaction and signing authority. Exact retries
// disclose the retained checkpoint only while the requesting enrollment
// remains live. A relay that is not the root forwards its installed
// checkpoint instead.
func (p RelayResetProtocol) CreateReset(ctx context.Context, bindingUID string, signer RootAttributionSigner) (RelayResetCheckpoint, error) {
	grant, err := p.Backend.RelayGrant(ctx, bindingUID, "pull")
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	if grant.ID <= 0 {
		return RelayResetCheckpoint{}, errors.New("root reset generation requires a downstream enrollment")
	}
	project, err := p.Backend.Project(ctx, *grant.ProjectID)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	if !ProjectAttributionVisible(ctx, project.UID) {
		return RelayResetCheckpoint{}, ErrNotFound
	}
	pin, err := p.Backend.RootPin(ctx, project.UID, "")
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	if pin.AuthorityUID == p.InstanceUID && (signer.AuthorityUID != p.InstanceUID || len(signer.PrivateKey) != ed25519.PrivateKeySize || RootPublicKeyID(signer.PrivateKey.Public().(ed25519.PublicKey)) != pin.KeyID) {
		return RelayResetCheckpoint{}, &RootSigningKeyError{Project: project.Name}
	}
	create := relayResetCreate{bindingUID: bindingUID, grant: grant, project: project, pin: pin, key: RelayResetMetadataPrefix + project.UID + "." + bindingUID}
	cached, found, err := p.Backend.Meta(ctx, create.key)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	if found {
		result, refresh, err := p.retainedCheckpoint(ctx, create, cached)
		if err != nil {
			return RelayResetCheckpoint{}, err
		}
		if !refresh {
			return p.AuthorizeCheckpointDownload(ctx, bindingUID, result)
		}
	}
	if pin.AuthorityUID == p.InstanceUID {
		// An authoritative root can snapshot its current state and carry
		// every retained delivery into the activated checkpoint epoch.
		return p.captureRootCheckpoint(ctx, create, signer)
	}
	// A relay hop forwards only what the root already owns. The same per-row
	// rule as installation applies, whether this is the first forward or a
	// rebuild of a retained one: an event the root has not receipted blocks,
	// while artifact offers and receipts are supersedable and never strand a
	// downstream whose superseded rows the old epoch no longer offers.
	deliveries, err := p.Backend.RelayUnacknowledgedDeliveries(ctx, bindingUID, grant.RelayResetEpoch)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	if err := RelayDownstreamOwesRootWork(grant, deliveries, nil); err != nil {
		return RelayResetCheckpoint{}, err
	}
	prepared, err := p.PrepareForwardedCheckpoint(ctx, grant, pin)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	return p.AuthorizeCheckpointDownload(ctx, bindingUID, prepared)
}

// retainedCheckpoint verifies the checkpoint retained for the enrollment and
// reports whether it must be rebuilt: a later root purge or installed
// upstream checkpoint made its snapshot obsolete, or history grew past a
// checkpoint the grant has not activated yet.
func (p RelayResetProtocol) retainedCheckpoint(ctx context.Context, create relayResetCreate, cached string) (RelayResetCheckpoint, bool, error) {
	var result RelayResetCheckpoint
	if err := json.Unmarshal([]byte(cached), &result, json.RejectUnknownMembers(true)); err != nil {
		return RelayResetCheckpoint{}, false, err
	}
	if err := RelayResetRetryEpoch(result.Translation.Authority.Epoch, create.grant.RelayResetEpoch); err != nil {
		return RelayResetCheckpoint{}, false, err
	}
	authority := RelayHopAuthority{BindingUID: create.bindingUID, ProjectUID: create.project.UID, AuthorityUID: create.pin.AuthorityUID, SenderInstanceUID: p.InstanceUID, ReceiverInstanceUID: create.grant.SpokeInstanceUID, Epoch: result.Translation.Authority.Epoch}
	if err := ValidateRelayResetTranslation(authority, result.Manifest, result.Translation); err != nil {
		return RelayResetCheckpoint{}, false, err
	}
	historical, err := p.Backend.RootPin(ctx, create.project.UID, result.Manifest.KeyID)
	if err != nil {
		return RelayResetCheckpoint{}, false, err
	}
	if historical.AuthorityUID != create.pin.AuthorityUID {
		return RelayResetCheckpoint{}, false, errors.New("cached reset changes pinned root authority")
	}
	if err := VerifyRootResetManifest(historical, result.Manifest, result.Snapshot); err != nil {
		return RelayResetCheckpoint{}, false, err
	}
	refresh, err := p.Backend.RelayCheckpointNeedsRefresh(ctx, create.project.UID, result)
	if err != nil {
		return RelayResetCheckpoint{}, false, err
	}
	if !refresh && result.Translation.Authority.Epoch == create.grant.RelayResetEpoch+1 {
		latest, err := p.Backend.LatestEventID(ctx, create.project.ID)
		if err != nil {
			return RelayResetCheckpoint{}, false, err
		}
		refresh = latest > result.Manifest.HistoryEventID
	}
	return result, refresh, nil
}

// captureRootCheckpoint writes the current-state records, serializes the
// project's portable history and signs the checkpoint for the enrollment.
func (p RelayResetProtocol) captureRootCheckpoint(ctx context.Context, create relayResetCreate, signer RootAttributionSigner) (RelayResetCheckpoint, error) {
	grant, project, pin := create.grant, create.project, create.pin
	// Existing baseline events advance the same durable HLC as later root writes.
	// Snapshot labels remain state; only the original signed creation references
	// below confer creator verification. Captured records do not create echo work.
	capture := WithRelayStateCapture(ctx)
	boundary, err := p.Backend.InsertFederationBaselineEvents(capture, project, grant.Actor)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	payload, err := ProjectMetadataAdoptionPayload(project.Metadata)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	clock := EventHLCTimestamp{PhysicalMS: boundary.HLCPhysicalMS, Counter: boundary.HLCCounter}
	state := RelayCheckpointStateEvent{Project: project, Type: "project.metadata_updated", Actor: grant.Actor, Payload: payload, Clock: clock, CreatedAt: boundary.CreatedAt}
	if err := p.Backend.InsertRelayCheckpointStateEvent(capture, state); err != nil {
		return RelayResetCheckpoint{}, err
	}
	// Read the existing portable history once. The new baseline's current entity
	// records are committed separately from immutable source history.
	records, err := p.Backend.RelayCheckpointRecords(ctx, project, boundary.ID)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	snapshot, err := records.Snapshot()
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	size := RelayCheckpointSize{Bytes: RootResetSnapshotBytes(snapshot), Capacity: p.Backend.RelayCheckpointCapacity()}
	if err := size.CapacityError(project.UID); err != nil {
		return RelayResetCheckpoint{}, err
	}
	if _, _, err := DecodeRootResetPayload(pin, snapshot); err != nil {
		return RelayResetCheckpoint{}, err
	}
	retained, err := p.Backend.MetaValuesWithPrefix(ctx, RelayResetMetadataPrefix+project.UID+".")
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	rootEpoch, err := NextRootResetEpoch(retained)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	snapshotUID, err := uid.New()
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	var baselines RelayStreamCursors
	for stream, target := range map[string]*int64{RelayStreamEvent: &baselines.Events, RelayStreamReceipt: &baselines.Receipts, RelayStreamArtifact: &baselines.Artifacts} {
		if *target, err = p.Backend.RelayOutboxMaxID(ctx, project.UID, stream); err != nil {
			return RelayResetCheckpoint{}, err
		}
	}
	historyID, err := p.Backend.LatestEventID(ctx, project.ID)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	manifest, err := SignRootResetManifest(RootResetManifest{Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, ResetEpoch: rootEpoch, SnapshotUID: snapshotUID, RootBaselines: baselines, HistoryEventID: historyID}, snapshot, signer.PrivateKey)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	newEpoch := grant.RelayResetEpoch + 1
	translation := RelayResetTranslation{Authority: RelayHopAuthority{BindingUID: create.bindingUID, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: p.InstanceUID, ReceiverInstanceUID: grant.SpokeInstanceUID, Epoch: newEpoch}, SnapshotUID: snapshotUID, SnapshotDigest: manifest.SnapshotDigest}
	result := RelayResetCheckpoint{Manifest: manifest, Snapshot: snapshot, Translation: translation}
	raw, err := json.Marshal(result)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	if err := p.Backend.PutMeta(ctx, create.key, string(raw)); err != nil {
		return RelayResetCheckpoint{}, err
	}
	grant.RelayResetEpoch = newEpoch
	if err := p.Backend.SeedRelayArtifactManifests(ctx, grant, project.UID); err != nil {
		return RelayResetCheckpoint{}, err
	}
	return p.AuthorizeCheckpointDownload(ctx, create.bindingUID, result)
}
