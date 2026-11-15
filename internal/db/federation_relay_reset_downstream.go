package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

// RelayDeliveryRow is one unacknowledged outbox delivery owed to a downstream
// grant, with whether the root has already receipted its source.
type RelayDeliveryRow struct {
	Stream     string
	SourceUID  string
	SourceHash string
	HasReceipt bool
}

// RelayOutboxEpochSummary describes one downstream grant's outbox epoch at
// the moment a forwarded reset supersedes it.
type RelayOutboxEpochSummary struct {
	// ThroughID is the greatest outbox row ID retained for the epoch.
	ThroughID               int64
	UnacknowledgedEvents    int
	UnacknowledgedArtifacts int
}

// RelayRetainedPostCheckpoint holds root-accepted work that arrived after the
// installed checkpoint was captured and therefore is absent from its snapshot.
type RelayRetainedPostCheckpoint struct {
	Events   []RemoteEvent
	Receipts []AttributionReceipt
}

// RelayDownstreamOwesRootWork refuses to move a downstream grant into a new
// epoch while it holds an unacknowledged delivery the installed checkpoint
// cannot stand in for; RelayDeliverySupersedable decides per row. The error
// names the enrollment and stream so an operator can let it synchronize or
// revoke it.
func RelayDownstreamOwesRootWork(grant FederationEnrollment, deliveries []RelayDeliveryRow, rootAccepted map[string]string) error {
	for _, delivery := range deliveries {
		if RelayDeliverySupersedable(delivery.Stream, delivery.SourceUID, delivery.SourceHash, delivery.HasReceipt, rootAccepted) {
			continue
		}
		return fmt.Errorf("%w: downstream enrollment %s still holds %s deliveries the root has not accepted; let it synchronize or revoke it, then retry", ErrFederationResetBlockedByPendingPush, grant.RelayBindingUID, delivery.Stream)
	}
	return nil
}

// DownstreamGrants lists the active downstream enrollments that a root reset
// installation carries into a new epoch. An enrollment still owed event
// deliveries the root has not accepted cannot be transitioned: no checkpoint
// stands in for that work, so installation is refused until the downstream
// synchronizes or is revoked.
func (p RelayResetProtocol) DownstreamGrants(ctx context.Context, projectID int64, rootAccepted map[string]string) ([]FederationEnrollment, error) {
	grants, err := p.Backend.RelayActiveDownstreamGrants(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, grant := range grants {
		deliveries, err := p.Backend.RelayUnacknowledgedDeliveries(ctx, grant.RelayBindingUID, grant.RelayResetEpoch)
		if err != nil {
			return nil, err
		}
		if err := RelayDownstreamOwesRootWork(grant, deliveries, rootAccepted); err != nil {
			return nil, err
		}
	}
	return grants, nil
}

// ForwardToDownstream prepares one downstream enrollment's next epoch from
// the checkpoint this relay just installed, without checking the child's
// credential: a suspended child must not block its relay. Event and artifact deliveries the downstream had not
// acknowledged are superseded by that checkpoint: they stay in the outbox as
// history and the recorded boundary keeps them from being offered again.
// Receipts stay owed, and root-accepted work newer than the checkpoint is
// queued in the new epoch.
func (p RelayResetProtocol) ForwardToDownstream(ctx context.Context, grant FederationEnrollment, pin RootKeyPin, retained RelayRetainedPostCheckpoint) error {
	if grant.ProjectID == nil {
		return ErrNotFound
	}
	summary, err := p.Backend.RelayOutboxEpochSummary(ctx, grant.RelayBindingUID, grant.RelayResetEpoch)
	if err != nil {
		return fmt.Errorf("count superseded relay deliveries: %w", err)
	}
	record := RelayResetSupersededDeliveries{Epoch: grant.RelayResetEpoch, ThroughID: summary.ThroughID, Events: summary.UnacknowledgedEvents, Artifacts: summary.UnacknowledgedArtifacts}
	// An earlier installation may already have seeded the next epoch from a
	// checkpoint that is now obsolete. Nothing in it was emitted, because the
	// first new-epoch request would have activated the grant, so replace it
	// whole. A re-forward of the same installed checkpoint keeps its seed.
	if err := p.Backend.DeleteUnemittedRelayOutbox(ctx, grant.RelayBindingUID, grant.RelayResetEpoch+1); err != nil {
		return fmt.Errorf("discard superseded forward seed: %w", err)
	}
	checkpoint, err := p.PrepareForwardedCheckpoint(ctx, grant, pin)
	if err != nil {
		return err
	}
	record.NextEpoch = checkpoint.Translation.Authority.Epoch
	record.SnapshotUID = checkpoint.Manifest.SnapshotUID
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := p.Backend.PutMeta(ctx, RelayResetSupersededMetadataKey(pin.ProjectUID, grant.RelayBindingUID), string(raw)); err != nil {
		return fmt.Errorf("record superseded relay deliveries: %w", err)
	}
	grant.RelayResetEpoch = record.NextEpoch
	for _, event := range retained.Events {
		body, err := EncodeRelaySourceEvent(event)
		if err != nil {
			return err
		}
		queued := RelayQueuedBody{ProjectID: *grant.ProjectID, ProjectUID: pin.ProjectUID, Stream: RelayStreamEvent, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body, Grant: grant}
		if err := p.Backend.QueueRelayBody(ctx, queued); err != nil {
			return err
		}
	}
	for _, receipt := range retained.Receipts {
		body, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		queued := RelayQueuedBody{ProjectID: *grant.ProjectID, ProjectUID: pin.ProjectUID, Stream: RelayStreamReceipt, SourceUID: receipt.EventUID, SourceHash: receipt.ContentHash, Body: body, Grant: grant}
		if err := p.Backend.QueueRelayBody(ctx, queued); err != nil {
			return err
		}
	}
	return nil
}
