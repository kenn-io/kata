package db

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"time"

	"go.kenn.io/kata/internal/embedding"
)

// RelayRetainedArtifactRow is one retained, unexpired embedding artifact of a
// project together with the current title and body of its issue.
type RelayRetainedArtifactRow struct {
	Artifact string
	Title    string
	Body     string
}

// RelayCursorSeed is the position one hop stream starts from in a new epoch.
type RelayCursorSeed struct {
	ProjectUID string
	BindingUID string
	Stream     string
	Epoch      int64
	Baseline   int64
}

// RelayResetAcceptedEvents returns the events a checkpoint proves the root
// accepted, keyed by event UID with the accepted content hash: each receipt
// issued by the pinned authority for this project whose hash matches the
// event the checkpoint carries. Deliveries of these events may be superseded
// by the checkpoint instead of blocking its installation.
func RelayResetAcceptedEvents(projectUID, authorityUID string, events []RemoteEvent, receipts []AttributionReceipt) map[string]string {
	hashes := make(map[string]string, len(events))
	for _, event := range events {
		hashes[event.EventUID] = event.ContentHash
	}
	accepted := make(map[string]string, len(receipts))
	for _, receipt := range receipts {
		if receipt.ProjectUID == projectUID && receipt.AuthorityUID == authorityUID && hashes[receipt.EventUID] == receipt.ContentHash {
			accepted[receipt.EventUID] = receipt.ContentHash
		}
	}
	return accepted
}

// RelayResetSupersedes decides whether a checkpoint may replace the one a
// relay retained for the project. A later root epoch always may; the same
// epoch may only when the manifest is identical; an earlier epoch is stale.
func RelayResetSupersedes(retained, next RootResetManifest) error {
	if next.ResetEpoch < retained.ResetEpoch {
		return errors.New("stale or conflicting root reset checkpoint")
	}
	if next.ResetEpoch > retained.ResetEpoch {
		return nil
	}
	previous, err := json.Marshal(retained)
	if err != nil {
		return err
	}
	current, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if string(previous) != string(current) {
		return errors.New("stale or conflicting root reset checkpoint")
	}
	return nil
}

// relayResetInstall is a verified checkpoint installation about to replace
// the project's projection: everything the apply phase needs.
type relayResetInstall struct {
	bindingUID    string
	projectID     int64
	projectUID    string
	pin           RootKeyPin
	events        []RemoteEvent
	provenance    RootResetProvenance
	retained      RelayRetainedPostCheckpoint
	downstream    []FederationEnrollment
	config        RelayBindingConfig
	checkpoint    RelayResetCheckpoint
	rawCheckpoint string
}

// InstallReset verifies the pinned root and current upstream authority inside
// the same transaction that replaces the projection and stream baselines.
// Work the root has not accepted blocks replacement before any mutation;
// active downstream enrollments are moved to a forwarded checkpoint epoch
// instead.
func (p RelayResetProtocol) InstallReset(ctx context.Context, bindingUID string, manifest RootResetManifest, snapshot RootResetSnapshot, translation RelayResetTranslation) error {
	if RootResetSnapshotBytes(snapshot) > MaxRelayEnvelopeBytes {
		return fmt.Errorf("%w: reset exceeds byte limit", ErrFederationIngestValidation)
	}
	grant, err := p.Backend.RelayGrant(ctx, bindingUID, "push")
	if err != nil {
		return err
	}
	if grant.ID != 0 {
		return errors.New("reset installation requires the configured upstream hop")
	}
	install := relayResetInstall{bindingUID: bindingUID, projectID: *grant.ProjectID}
	install.projectUID, err = p.Backend.ProjectUID(ctx, install.projectID)
	if err != nil {
		return err
	}
	if !ProjectAttributionVisible(ctx, install.projectUID) {
		return ErrNotFound
	}
	install.pin, err = p.Backend.RootPin(ctx, install.projectUID, "")
	if err != nil {
		return err
	}
	historical, err := p.Backend.RootPin(ctx, install.projectUID, manifest.KeyID)
	if err != nil {
		return err
	}
	if historical.AuthorityUID != install.pin.AuthorityUID {
		return errors.New("reset changes pinned root authority")
	}
	if err := VerifyRootResetManifest(historical, manifest, snapshot); err != nil {
		return err
	}
	authority := RelayHopAuthority{BindingUID: bindingUID, ProjectUID: install.projectUID, AuthorityUID: install.pin.AuthorityUID, SenderInstanceUID: grant.SpokeInstanceUID, ReceiverInstanceUID: p.InstanceUID, Epoch: translation.Authority.Epoch}
	if err := ValidateRelayResetTranslation(authority, manifest, translation); err != nil {
		return err
	}
	install.events, install.provenance, err = DecodeRootResetPayload(historical, snapshot)
	if err != nil {
		return err
	}
	snapshotEvents, snapshotReceipts := len(install.events), len(install.provenance.Receipts)
	install.events, err = p.RetainPostCheckpointEvents(ctx, install.projectID, install.projectUID, historical, install.events, &install.provenance)
	if err != nil {
		return err
	}
	install.retained = RelayRetainedPostCheckpoint{Events: install.events[snapshotEvents:], Receipts: install.provenance.Receipts[snapshotReceipts:]}
	rootAccepted := RelayResetAcceptedEvents(install.projectUID, install.pin.AuthorityUID, install.events, install.provenance.Receipts)
	install.checkpoint = RelayResetCheckpoint{Manifest: manifest, Snapshot: snapshot, Translation: translation}
	raw, err := json.Marshal(install.checkpoint)
	if err != nil {
		return err
	}
	install.rawCheckpoint = string(raw)
	previous, found, err := p.Backend.Meta(ctx, RelayResetMetadataPrefix+install.projectUID)
	if err != nil {
		return err
	}
	if found {
		if previous == install.rawCheckpoint && translation.Authority.Epoch == grant.RelayResetEpoch {
			return nil
		}
		var old RelayResetCheckpoint
		if err := json.Unmarshal([]byte(previous), &old, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		if err := RelayResetSupersedes(old.Manifest, manifest); err != nil {
			return err
		}
	}
	if translation.Authority.Epoch <= grant.RelayResetEpoch {
		return errors.New("stale immediate-hop reset epoch")
	}
	binding, err := p.Backend.RelayBinding(ctx, install.projectID)
	if err != nil {
		return err
	}
	if binding.RelayConfig == nil {
		return ErrNotFound
	}
	if err := p.ValidateUpstreamWork(ctx, install.projectID, install.projectUID, binding.RelayConfig, rootAccepted); err != nil {
		return err
	}
	// Active downstream enrollments follow the relay into the new epoch
	// instead of blocking it; only work the root never accepted refuses.
	install.downstream, err = p.DownstreamGrants(ctx, install.projectID, rootAccepted)
	if err != nil {
		return err
	}
	if err := p.installBlockers(ctx, install.projectID); err != nil {
		return err
	}
	if err := p.Backend.ValidateRelayResetEvents(install.projectUID, install.events); err != nil {
		return err
	}
	install.config = *binding.RelayConfig
	install.config.ResetEpoch = translation.Authority.Epoch
	return p.applyReset(ctx, install)
}

// installBlockers refuses an installation while the project holds quarantined
// deliveries or an external root binding; neither state survives a projection
// replacement.
func (p RelayResetProtocol) installBlockers(ctx context.Context, projectID int64) error {
	quarantined, err := p.Backend.RelayQuarantineOpen(ctx, projectID)
	if err != nil {
		return err
	}
	if quarantined {
		return ErrFederationResetBlockedByQuarantine
	}
	external, err := p.Backend.ExternalRootBindingExists(ctx, projectID)
	if err != nil {
		return err
	}
	if external {
		return ErrFederationResetBlockedByExternalRoot
	}
	return nil
}

// applyReset replaces the project's history, projection and provenance with
// the verified checkpoint, then installs the new hop epoch and forwards the
// checkpoint downstream in the same transaction.
func (p RelayResetProtocol) applyReset(ctx context.Context, install relayResetInstall) error {
	// The reset's imported events must not be echoed as fresh local intent.
	// They are inserted with original IDs/hashes, independently of hop sequencing.
	// Daemon-local catalog history, such as renames and alias removals, has no
	// root receipt and is not in the snapshot, so it stays. Every other record,
	// including anything a root receipt attests, is replaced.
	if err := p.Backend.ReplaceRelayHistory(ctx, install.projectID, install.projectUID); err != nil {
		return err
	}
	if err := p.Backend.DeleteProjectProvenance(ctx, install.projectUID); err != nil {
		return err
	}
	for _, key := range install.provenance.Keys {
		retained, err := p.Backend.RootPin(ctx, install.projectUID, key.KeyID)
		if err == nil {
			if retained.AuthorityUID != key.AuthorityUID || retained.KeyID != key.KeyID {
				return errors.New("reset historical key conflicts with retained root")
			}
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if !key.Retired {
			return errors.New("reset cannot replace the active root")
		}
		if err := p.Backend.InsertRetiredRootKey(ctx, key); err != nil {
			return err
		}
	}
	for _, event := range install.events {
		if err := p.Backend.InsertRelayResetEvent(ctx, install.projectID, event); err != nil {
			return err
		}
	}
	if err := p.Backend.MaterializeFederatedProject(ctx, install.projectID); err != nil {
		return err
	}
	// Projection replacement cannot leave portable bytes reachable for issues
	// excluded by the authoritative snapshot. Artifacts have a project FK,
	// so the issue deletion alone does not remove them.
	if err := p.Backend.DeleteOrphanRelayArtifacts(ctx, install.projectUID); err != nil {
		return err
	}
	if err := p.Backend.DeleteOrphanPendingCreations(ctx, install.projectID, install.projectUID); err != nil {
		return err
	}
	if err := p.retainResetArtifacts(ctx, install.projectUID, install.checkpoint.Snapshot.Artifacts); err != nil {
		return err
	}
	for _, receipt := range install.provenance.Receipts {
		bytes, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		if err := p.Backend.InsertEventProvenance(ctx, install.projectUID, receipt, string(bytes)); err != nil {
			return err
		}
	}
	for _, ref := range install.provenance.Entities {
		if err := p.Backend.InsertEntityProvenance(ctx, install.projectUID, ref); err != nil {
			return err
		}
		if err := p.Backend.DeletePendingCreation(ctx, install.projectUID, ref.Kind, ref.EntityUID); err != nil {
			return err
		}
	}
	return p.installEpoch(ctx, install)
}

// installEpoch moves the upstream hop to the checkpoint epoch, records the
// checkpoint and carries every downstream enrollment into its own new epoch.
func (p RelayResetProtocol) installEpoch(ctx context.Context, install relayResetInstall) error {
	configBytes, err := json.Marshal(install.config)
	if err != nil {
		return err
	}
	if err := p.Backend.UpdateRelayBindingConfig(ctx, install.projectID, string(configBytes)); err != nil {
		return err
	}
	baselines := install.checkpoint.Translation.HopBaselines
	for stream, baseline := range map[string]int64{RelayStreamEvent: baselines.Events, RelayStreamReceipt: baselines.Receipts, RelayStreamArtifact: baselines.Artifacts} {
		seed := RelayCursorSeed{ProjectUID: install.projectUID, BindingUID: install.bindingUID, Stream: stream, Epoch: install.config.ResetEpoch, Baseline: baseline}
		if err := p.Backend.InsertRelayCursor(ctx, seed); err != nil {
			return err
		}
	}
	if err := p.Backend.PutMeta(ctx, RelayResetMetadataPrefix+install.projectUID, install.rawCheckpoint); err != nil {
		return err
	}
	if err := p.Backend.ReserveAttributionUIReset(ctx, install.projectUID); err != nil {
		return err
	}
	// The installed checkpoint is forwarded to every downstream enrollment
	// in the same transaction, so a descendant can never observe a relay
	// projection without the epoch that carries it.
	for _, child := range install.downstream {
		if err := p.ForwardToDownstream(ctx, child, install.pin, install.retained); err != nil {
			return fmt.Errorf("forward relay reset to downstream enrollment %s: %w", child.RelayBindingUID, err)
		}
	}
	// Time-dependent authority is rechecked at commit; binding/policy row fences
	// held by the upstream grant load retain the account and membership decision.
	_, err = p.Backend.UpstreamRelayGrant(ctx, install.bindingUID)
	return err
}

// retainResetArtifacts keeps the staged embedding artifacts the checkpoint's
// manifests describe. Metadata alone cannot supply missing or expired bytes;
// retained bytes must match their manifest exactly, and stale bytes remain
// portable without becoming active-index evidence.
func (p RelayResetProtocol) retainResetArtifacts(ctx context.Context, projectUID string, manifests []byte) error {
	var offered []embedding.ArtifactManifest
	if err := json.Unmarshal(manifests, &offered, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	for _, manifest := range offered {
		row, found, err := p.Backend.RetainedRelayArtifact(ctx, projectUID, manifest.Digest, time.Now())
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		var artifact embedding.EmbeddingArtifact
		input := embedding.EmbedText(row.Title, row.Body)
		if manifest.InputHash != embedding.ArtifactInputHash(input) {
			input = ""
		}
		if json.Unmarshal([]byte(row.Artifact), &artifact, json.RejectUnknownMembers(true)) != nil || embedding.ValidateArtifact(artifact, input, "") != nil || !reflect.DeepEqual(artifact.Manifest(), manifest) {
			return ErrFederationIngestValidation
		}
		if err := p.Backend.ActivateStagedRelayArtifact(ctx, projectUID, manifest.Digest); err != nil {
			return err
		}
	}
	return nil
}
