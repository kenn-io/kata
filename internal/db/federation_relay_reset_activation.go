package db

import (
	"context"
	"encoding/json/v2"
)

// SupersededDeliveries loads the record of deliveries a forwarded root reset
// superseded for one downstream grant. Absence means nothing is hidden.
func (p RelayResetProtocol) SupersededDeliveries(ctx context.Context, projectUID, bindingUID string) (RelayResetSupersededDeliveries, bool, error) {
	raw, found, err := p.Backend.Meta(ctx, RelayResetSupersededMetadataKey(projectUID, bindingUID))
	if err != nil || !found {
		return RelayResetSupersededDeliveries{}, false, err
	}
	var record RelayResetSupersededDeliveries
	if err := json.Unmarshal([]byte(raw), &record, json.RejectUnknownMembers(true)); err != nil {
		return RelayResetSupersededDeliveries{}, false, err
	}
	return record, true, nil
}

// RelayActivationRequested decides whether a peer's requested epoch activates
// the grant's prepared checkpoint. No request, or the current epoch, keeps the
// grant where it is; only the next epoch activates; anything else is an
// invalid request.
func RelayActivationRequested(requested, current int64) (bool, error) {
	if requested == 0 || requested == current {
		return false, nil
	}
	if requested != current+1 {
		return false, ErrFederationIngestValidation
	}
	return true, nil
}

// ActivateCheckpoint moves a grant into its prepared checkpoint epoch on the
// first authenticated new-epoch operation. Preparing a checkpoint does not
// retire the peer's old namespace: late old-epoch intent can still drain,
// preserving its emitted envelopes and source identity.
func (p RelayResetProtocol) ActivateCheckpoint(ctx context.Context, grant FederationEnrollment) (FederationEnrollment, error) {
	requested := RelayRequestedEpoch(ctx)
	activate, err := RelayActivationRequested(requested, grant.RelayResetEpoch)
	if err != nil || !activate {
		return grant, err
	}
	projectUID, err := p.Backend.ProjectUID(ctx, *grant.ProjectID)
	if err != nil {
		return grant, err
	}
	if !ProjectAttributionVisible(ctx, projectUID) {
		return grant, ErrNotFound
	}
	raw, found, err := p.Backend.Meta(ctx, RelayResetMetadataPrefix+projectUID+"."+grant.RelayBindingUID)
	if err != nil || !found {
		return grant, ErrFederationIngestValidation
	}
	var checkpoint RelayResetCheckpoint
	if err := json.Unmarshal([]byte(raw), &checkpoint, json.RejectUnknownMembers(true)); err != nil {
		return grant, err
	}
	pin, err := p.Backend.RootPin(ctx, projectUID, checkpoint.Manifest.KeyID)
	if err != nil {
		return grant, err
	}
	authority := RelayHopAuthority{BindingUID: grant.RelayBindingUID, ProjectUID: projectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: p.InstanceUID, ReceiverInstanceUID: grant.SpokeInstanceUID, Epoch: requested}
	if err := ValidateRelayResetTranslation(authority, checkpoint.Manifest, checkpoint.Translation); err != nil {
		return grant, err
	}
	if err := VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot); err != nil {
		return grant, err
	}
	// Writes committed after preparation remain owed. Reoffer them in the new
	// namespace; leave old emitted records untouched rather than fabricating ACKs.
	// Event and artifact rows a forwarded root reset superseded are history:
	// the checkpoint already carries the root's state for them, so they are
	// never reoffered. Receipts stay owed and follow the grant.
	superseded, recorded, err := p.SupersededDeliveries(ctx, projectUID, grant.RelayBindingUID)
	if err != nil {
		return grant, err
	}
	boundary := superseded.BoundaryFor(RelayStreamEvent, grant.RelayResetEpoch, requested)
	rawEnvelopes, err := p.Backend.RelayUnacknowledgedEnvelopes(ctx, grant.RelayBindingUID, grant.RelayResetEpoch, RelayStreamReceipt, boundary)
	if err != nil {
		return grant, err
	}
	retained := make([]RelayEnvelope, 0, len(rawEnvelopes))
	for _, bytes := range rawEnvelopes {
		var envelope RelayEnvelope
		if err := json.Unmarshal([]byte(bytes), &envelope, json.RejectUnknownMembers(true)); err != nil {
			return grant, err
		}
		retained = append(retained, envelope)
	}
	if err := p.Backend.SetRelayGrantEpoch(ctx, grant.ID, requested); err != nil {
		return grant, err
	}
	// The record served its purpose once the superseded epoch is left behind.
	if recorded && superseded.Epoch == grant.RelayResetEpoch {
		if err := p.Backend.DeleteMeta(ctx, RelayResetSupersededMetadataKey(projectUID, grant.RelayBindingUID)); err != nil {
			return grant, err
		}
	}
	grant.RelayResetEpoch = requested
	for _, envelope := range retained {
		queueCtx := WithRelayArtifactSourceUID(WithRelayForwardPath(ctx, envelope.Path), envelope.SourceUID)
		body := RelayQueuedBody{ProjectID: *grant.ProjectID, ProjectUID: projectUID, Stream: envelope.Stream, SourceUID: envelope.SourceUID, SourceHash: envelope.SourceHash, Body: envelope.Body, Grant: grant}
		if err := p.Backend.QueueRelayBody(queueCtx, body); err != nil {
			return grant, err
		}
	}
	return grant, nil
}
