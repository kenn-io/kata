package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
)

// relayResetSeedPageSize bounds one page of retained deliveries a forward
// reseeds into a child's epoch.
const relayResetSeedPageSize = 100

// RelayRetainedEnvelopeRow is one retained inbox or outbox delivery as the
// backend loads it: the row's position and source identity plus the raw
// envelope. Inbox rows carry the hop sequence; outbox rows carry the row ID,
// which the outbox assigns as the envelope sequence.
type RelayRetainedEnvelopeRow struct {
	Sequence   int64
	SourceUID  string
	SourceHash string
	Envelope   string
}

// RelayRetainedPage selects one page of retained deliveries after a position.
type RelayRetainedPage struct {
	ProjectUID string
	BindingUID string
	Stream     string
	Epoch      int64
	After      int64
	Limit      int
}

// RelayRetainedDeliveryCheck validates retained delivery rows from one hop
// before a forward reseeds them into a child's epoch.
type RelayRetainedDeliveryCheck struct {
	// Authority is the hop the rows were accepted or sent under.
	Authority RelayHopAuthority
	// Stream is the stream the rows were loaded from.
	Stream string
	// ProjectUID is the project every event body must belong to.
	ProjectUID string
	// Label names the retained store in failure messages.
	Label string
	// OriginFromPath requires an event body's origin to be the first hop in
	// the envelope path, as every envelope this hub sent upstream records.
	OriginFromPath bool
}

// Envelope decodes one row and checks that the row columns, the stream and
// the authenticated hop agree with the envelope. An event body must carry
// the row's source identity for the project.
func (c RelayRetainedDeliveryCheck) Envelope(row RelayRetainedEnvelopeRow) (RelayEnvelope, error) {
	var envelope RelayEnvelope
	if err := json.Unmarshal([]byte(row.Envelope), &envelope, json.RejectUnknownMembers(true)); err != nil {
		return RelayEnvelope{}, err
	}
	if envelope.Sequence != row.Sequence || envelope.Stream != c.Stream || envelope.SourceUID != row.SourceUID || envelope.SourceHash != row.SourceHash || ValidateRelayEnvelope(c.Authority, envelope) != nil {
		return RelayEnvelope{}, fmt.Errorf("%w: retained %s relay envelope is inconsistent", ErrFederationIngestValidation, c.Label)
	}
	if c.Stream != RelayStreamEvent {
		return envelope, nil
	}
	source, err := DecodeRelaySourceEvent(envelope.Body)
	if err != nil || source.ProjectUID != c.ProjectUID || source.EventUID != row.SourceUID || source.ContentHash != row.SourceHash || (c.OriginFromPath && source.OriginInstanceUID != envelope.Path[0]) {
		return RelayEnvelope{}, fmt.Errorf("%w: retained %s relay source is inconsistent", ErrFederationIngestValidation, c.Label)
	}
	return envelope, nil
}

// RelayResetForwardTranslation translates an installed root checkpoint to one
// downstream hop. The signed manifest and snapshot are forwarded unchanged;
// only the immediate-hop translation is replaced: the child's binding, this
// instance as sender, the grant's next epoch and zero hop baselines, since
// upstream baselines mean nothing on the child's hop.
func RelayResetForwardTranslation(checkpoint RelayResetCheckpoint, grant FederationEnrollment, pin RootKeyPin, instanceUID string) RelayResetCheckpoint {
	authority := RelayHopAuthority{BindingUID: grant.RelayBindingUID, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: instanceUID, ReceiverInstanceUID: grant.SpokeInstanceUID, Epoch: grant.RelayResetEpoch + 1}
	checkpoint.Translation = RelayResetTranslation{Authority: authority, SnapshotUID: checkpoint.Manifest.SnapshotUID, SnapshotDigest: checkpoint.Manifest.SnapshotDigest}
	return checkpoint
}

// PrepareForwardedCheckpoint translates the relay's durably installed root
// checkpoint for one downstream grant and seeds that grant's next epoch.
// Only the immediate-hop translation changes; accepted post-checkpoint
// deliveries are carried into the child's new epoch.
//
// Preparation is the relay's own bookkeeping and never checks the child's
// credential: a child whose parent token expired or was revoked must not stop
// its relay from installing a root reset, and keeps recoverable state if the
// credential returns. Disclosure is authorized separately, by
// AuthorizeCheckpointDownload and by the enrollment fence that guards
// activation of the prepared epoch.
func (p RelayResetProtocol) PrepareForwardedCheckpoint(ctx context.Context, grant FederationEnrollment, pin RootKeyPin) (RelayResetCheckpoint, error) {
	binding, err := p.Backend.RelayServingBinding(ctx, *grant.ProjectID)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	if binding.RelayConfig == nil {
		return RelayResetCheckpoint{}, ErrNotFound
	}
	upstream, err := p.Backend.UpstreamRelayGrant(ctx, binding.RelayConfig.BindingUID)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	raw, found, err := p.Backend.Meta(ctx, RelayResetMetadataPrefix+pin.ProjectUID)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	if !found {
		return RelayResetCheckpoint{}, fmt.Errorf("relay has no installed root checkpoint to forward: %w", sql.ErrNoRows)
	}
	var checkpoint RelayResetCheckpoint
	if err := json.Unmarshal([]byte(raw), &checkpoint, json.RejectUnknownMembers(true)); err != nil {
		return RelayResetCheckpoint{}, err
	}
	historical, err := p.Backend.RootPin(ctx, pin.ProjectUID, checkpoint.Manifest.KeyID)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	if historical.AuthorityUID != pin.AuthorityUID {
		return RelayResetCheckpoint{}, errors.New("forwarded reset changes root authority")
	}
	if err := VerifyRootResetManifest(historical, checkpoint.Manifest, checkpoint.Snapshot); err != nil {
		return RelayResetCheckpoint{}, err
	}
	authority := RelayHopAuthority{BindingUID: binding.RelayConfig.BindingUID, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: upstream.SpokeInstanceUID, ReceiverInstanceUID: p.InstanceUID, Epoch: upstream.RelayResetEpoch}
	if err := ValidateRelayResetTranslation(authority, checkpoint.Manifest, checkpoint.Translation); err != nil {
		return RelayResetCheckpoint{}, err
	}
	if _, _, err := DecodeRootResetPayload(historical, checkpoint.Snapshot); err != nil {
		return RelayResetCheckpoint{}, err
	}
	source := relayForwardSource{BindingUID: binding.RelayConfig.BindingUID, Grant: upstream, Baselines: checkpoint.Translation.HopBaselines}
	checkpoint = RelayResetForwardTranslation(checkpoint, grant, pin, p.InstanceUID)
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return RelayResetCheckpoint{}, err
	}
	if err := p.Backend.PutMeta(ctx, RelayResetMetadataPrefix+pin.ProjectUID+"."+grant.RelayBindingUID, string(encoded)); err != nil {
		return RelayResetCheckpoint{}, err
	}
	grant.RelayResetEpoch = checkpoint.Translation.Authority.Epoch
	if err := p.Backend.SeedRelayArtifactManifests(ctx, grant, pin.ProjectUID); err != nil {
		return RelayResetCheckpoint{}, err
	}
	if err := p.seedPostCheckpointDeliveries(ctx, grant, source, pin); err != nil {
		return RelayResetCheckpoint{}, err
	}
	return checkpoint, nil
}

// AuthorizeCheckpointDownload rechecks the requesting enrollment's live
// authority (enrollment, parent credential, project access) immediately
// before a prepared or retained checkpoint is disclosed to it. It is the only
// authorization step of a checkpoint download; preparation never performs it.
func (p RelayResetProtocol) AuthorizeCheckpointDownload(ctx context.Context, bindingUID string, checkpoint RelayResetCheckpoint) (RelayResetCheckpoint, error) {
	if _, err := p.Backend.RelayGrant(ctx, bindingUID, "pull"); err != nil {
		return RelayResetCheckpoint{}, err
	}
	return checkpoint, nil
}

// relayForwardSource is the upstream hop a forward carries deliveries from:
// its binding, its live grant and the hop baselines of the installed
// checkpoint, beyond which deliveries are post-checkpoint.
type relayForwardSource struct {
	BindingUID string
	Grant      FederationEnrollment
	Baselines  RelayStreamCursors
}

// seedPostCheckpointDeliveries carries accepted upstream event and receipt
// envelopes beyond the installed checkpoint into this child's epoch. They
// remain ordinary post-snapshot deliveries and keep their original source
// bytes while receiving the child's immediate-hop identity.
func (p RelayResetProtocol) seedPostCheckpointDeliveries(ctx context.Context, grant FederationEnrollment, source relayForwardSource, pin RootKeyPin) error {
	if grant.ProjectID == nil {
		return ErrNotFound
	}
	inbound := RelayRetainedDeliveryCheck{Authority: RelayHopAuthority{BindingUID: source.BindingUID, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: source.Grant.SpokeInstanceUID, ReceiverInstanceUID: p.InstanceUID, Epoch: source.Grant.RelayResetEpoch}, ProjectUID: pin.ProjectUID, Label: "upstream"}
	streams := []struct {
		name     string
		baseline int64
	}{{RelayStreamEvent, source.Baselines.Events}, {RelayStreamReceipt, source.Baselines.Receipts}}
	for _, stream := range streams {
		inbound.Stream = stream.name
		page := RelayRetainedPage{ProjectUID: pin.ProjectUID, BindingUID: source.BindingUID, Stream: stream.name, Epoch: source.Grant.RelayResetEpoch, After: stream.baseline, Limit: relayResetSeedPageSize}
		for {
			rows, err := p.Backend.RelayAcceptedInboxPage(ctx, page)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				break
			}
			for _, row := range rows {
				envelope, err := inbound.Envelope(row)
				if err != nil {
					return err
				}
				path := append(slices.Clone(envelope.Path), p.InstanceUID)
				body := RelayQueuedBody{ProjectID: *grant.ProjectID, ProjectUID: pin.ProjectUID, Stream: stream.name, SourceUID: row.SourceUID, SourceHash: row.SourceHash, Body: envelope.Body, Grant: grant}
				if err := p.Backend.QueueRelayBody(WithRelayForwardPath(ctx, path), body); err != nil {
					return err
				}
				page.After = row.Sequence
			}
		}
	}
	// The upstream inbox contains root-origin deliveries, but local and other
	// downstream-origin events are retained in the outgoing upstream outbox.
	// Those envelopes already carry this hub in their relay path, so preserve
	// their path and body exactly when seeding a newly enrolled child.
	outbound := RelayRetainedDeliveryCheck{Authority: RelayHopAuthority{BindingUID: source.BindingUID, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: p.InstanceUID, ReceiverInstanceUID: source.Grant.SpokeInstanceUID, Epoch: source.Grant.RelayResetEpoch}, Stream: RelayStreamEvent, ProjectUID: pin.ProjectUID, Label: "outgoing", OriginFromPath: true}
	page := RelayRetainedPage{ProjectUID: pin.ProjectUID, BindingUID: source.BindingUID, Stream: RelayStreamEvent, Epoch: source.Grant.RelayResetEpoch, Limit: relayResetSeedPageSize}
	for {
		rows, err := p.Backend.RelayOutboxPage(ctx, page)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			envelope, err := outbound.Envelope(row)
			if err != nil {
				return err
			}
			body := RelayQueuedBody{ProjectID: *grant.ProjectID, ProjectUID: pin.ProjectUID, Stream: RelayStreamEvent, SourceUID: row.SourceUID, SourceHash: row.SourceHash, Body: envelope.Body, Grant: grant}
			if err := p.Backend.QueueRelayBody(WithRelayForwardPath(ctx, envelope.Path), body); err != nil {
				return err
			}
			page.After = row.Sequence
		}
	}
}
