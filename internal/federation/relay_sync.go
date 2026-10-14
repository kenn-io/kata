package federation

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/federationsigning"
)

// relaySyncTurnBatches bounds the batches one sync turn exchanges per stream
// and direction before it yields. The runner starts the next pass at once
// while deliveries remain, so one backlog delays the other streams, the other
// projects and local indexing by at most one bounded turn, never a full drain.
var relaySyncTurnBatches = 8

// relaySyncBatchLimit is the delivery count one event or receipt batch carries.
var relaySyncBatchLimit = federationPollLimit

// relaySyncTurn records that a budgeted loop stopped with work still pending.
type relaySyncTurn struct{ remaining bool }

// spent records one processed nonempty batch and reports whether the loop's
// turn budget is used up. Delivery counts, manifest bytes and vector bytes all
// cut batches short, so a short batch does not prove the stream is drained:
// every batch that uses up the budget leaves possible work, and only an empty
// batch, which ends the loop before this call, proves the stream empty.
func (t *relaySyncTurn) spent(batches int) bool {
	if batches < relaySyncTurnBatches {
		return false
	}
	t.remaining = true
	return true
}

// syncRelayBinding runs one turn inside the existing per-project sync
// coordination. The sole outbound connection carries both directions and
// independent streams; each stream loop stops at the turn budget.
func syncRelayBinding(ctx context.Context, store db.Storage, binding db.FederationBinding, remoteProjectID int64, client *Client, onPulledEvents func(int64, []db.Event), validateLease func(context.Context) error, turn *relaySyncTurn) error {
	c := binding.RelayConfig
	if err := c.Validate(store.InstanceUID()); err != nil {
		return err
	}
	if !binding.Enabled || !binding.PushEnabled {
		return db.ErrNotFound
	}
	project, err := store.ProjectByID(ctx, binding.ProjectID)
	if err != nil {
		return err
	}
	meta, err := client.ProjectFederation(ctx, remoteProjectID)
	if err != nil {
		return err
	}
	pin, err := store.RootAuthority(ctx, project.UID)
	if err != nil {
		return err
	}
	handshake := meta.Relay
	if handshake == nil || handshake.ProtocolVersion != db.RelayProtocolVersion || meta.ProjectUID != project.UID || project.UID != binding.HubProjectUID || handshake.BindingUID != c.BindingUID || handshake.UpstreamInstanceUID != c.UpstreamInstanceUID || handshake.Root.AuthorityUID != c.AuthorityUID || handshake.Root.ProjectUID != project.UID || !slices.Equal(handshake.HubPath, c.HubPath) {
		return errors.New("relay metadata differs from the pinned project, hop or authority")
	}
	transitions, err := relayRootTransitionPath(pin, handshake.Root, handshake.RootKeyTransitions)
	if err != nil {
		return err
	}
	recoveredRevocation := false
	if c.UpstreamRevoked {
		// Validate the complete signed transition chain before reopening the
		// recovered hop. The expected config keeps a concurrent operator change
		// or policy update from being overwritten during this recovery.
		if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
			return err
		}
		recovered := *c
		recovered.UpstreamRevoked = false
		binding, err = store.SetRelayBindingConfig(ctx, binding.ProjectID, recovered, *c)
		if err != nil {
			return err
		}
		c = binding.RelayConfig
		recoveredRevocation = true
	}
	for _, transition := range transitions {
		if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
			return err
		}
		if err := store.RotateRootAuthority(ctx, transition); err != nil {
			if recoveredRevocation {
				revoked := *c
				revoked.UpstreamRevoked = true
				if leaseErr := validateFederationRunnerLease(ctx, validateLease); leaseErr != nil {
					return errors.Join(err, leaseErr)
				}
				if _, restoreErr := store.SetRelayBindingConfig(ctx, binding.ProjectID, revoked, *c); restoreErr != nil {
					return errors.Join(err, restoreErr)
				}
			}
			return err
		}
	}
	// Projection installation may commit before the first new-epoch request
	// reaches the upstream. Resume that namespace instead of fetching another
	// snapshot; native upstream admission requires its retained checkpoint.
	if handshake.ResetRequired && handshake.ResetEpoch+1 == c.ResetEpoch {
		return syncRelayStreams(ctx, store, binding, remoteProjectID, client, onPulledEvents, validateLease, turn)
	}
	if handshake.ResetRequired && handshake.ResetEpoch == c.ResetEpoch {
		// A fresh signed baseline can contain state needed to apply queued root
		// mutations. Drain only this replica's outbound intent before requesting
		// it; incoming event and artifact deliveries resume after installation.
		if err := syncRelayPushBeforeReset(ctx, store, binding, remoteProjectID, client, validateLease, turn); err != nil {
			return err
		}
		if turn.remaining {
			// The drain stopped at the turn budget; request the checkpoint
			// on a later turn once this replica's intent is upstream.
			return nil
		}
	}
	if handshake.ResetRequired || handshake.ResetEpoch != c.ResetEpoch {
		installer, ok := store.(db.RelayResetStore)
		if !ok {
			return ErrFederationResetRequired
		}
		if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
			return err
		}
		checkpoint, err := client.RelayReset(ctx, remoteProjectID)
		if err != nil {
			return err
		}
		if (!handshake.ResetRequired && checkpoint.Translation.Authority.Epoch != handshake.ResetEpoch) || (handshake.ResetRequired && checkpoint.Translation.Authority.Epoch < c.ResetEpoch) {
			return errors.New("relay reset differs from advertised epoch")
		}
		if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
			return err
		}
		if err := installer.InstallRelayReset(ctx, c.BindingUID, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation); err != nil {
			return err
		}
		current, err := store.FederationBindingByProject(ctx, binding.ProjectID)
		if err != nil {
			return err
		}
		if current.RelayConfig == nil || current.RelayConfig.BindingUID != c.BindingUID || current.RelayConfig.ResetEpoch != checkpoint.Translation.Authority.Epoch {
			return errors.New("relay binding changed during reset")
		}
		binding = current
	}
	return syncRelayStreams(ctx, store, binding, remoteProjectID, client, onPulledEvents, validateLease, turn)
}

// Exchange pending deliveries in both directions with native acceptance and
// emitted-prefix ACK checks, at most the turn budget per stream and direction.
func syncRelayStreams(ctx context.Context, store db.Storage, binding db.FederationBinding, remoteProjectID int64, client *Client, onPulledEvents func(int64, []db.Event), validateLease func(context.Context) error, turn *relaySyncTurn) error {
	c := binding.RelayConfig
	quarantines, err := store.ActiveFederationQuarantinesByProject(ctx, binding.ProjectID)
	if err != nil {
		return err
	}
	if len(quarantines) > 0 {
		if quarantines[0].Direction == db.FederationQuarantineDirectionPull {
			return db.ErrFederationPullQuarantined
		}
		return db.ErrFederationPushQuarantined
	}
	if err := syncRelayPushEvents(ctx, store, binding, remoteProjectID, client, validateLease, turn); err != nil {
		return err
	}
	if err := store.RecordFederationSyncPushSuccess(ctx, binding.ProjectID, time.Now().UTC()); err != nil {
		return err
	}
	if err := store.RecordFederationSyncPullStarted(ctx, binding.ProjectID, time.Now().UTC()); err != nil {
		return err
	}
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt} {
		for batches := 1; ; batches++ {
			if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
				return err
			}
			batch, err := client.OfferRelayDeliveries(ctx, remoteProjectID, stream, relaySyncBatchLimit, c.ResetEpoch)
			if err != nil {
				return err
			}
			if batch.Stream != stream {
				return errors.New("relay offer uses a different stream")
			}
			if len(batch.Envelopes) == 0 {
				break
			}
			accepted, err := store.AcceptRelayDeliveries(ctx, c.BindingUID, batch)
			if err != nil {
				return recordRelayEventQuarantine(ctx, store, binding, db.FederationQuarantineDirectionPull, batch, err, validateLease)
			}
			if len(accepted.InsertedEvents) > 0 && onPulledEvents != nil {
				onPulledEvents(binding.ProjectID, accepted.InsertedEvents)
			}
			if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
				return err
			}
			if err := client.AckRelayDeliveries(ctx, remoteProjectID, c.ResetEpoch, stream, accepted); err != nil {
				return err
			}
			if turn.spent(batches) {
				break
			}
		}
	}
	if err := syncRelayArtifacts(ctx, store, binding, remoteProjectID, client, validateLease, turn); err != nil {
		return err
	}
	return store.RecordFederationSyncPullSuccess(ctx, binding.ProjectID, time.Now().UTC())
}

// syncRelayPushBeforeReset retains local event and artifact intent without
// applying upstream deliveries against a projection that the signed baseline
// has not installed yet.
func syncRelayPushBeforeReset(ctx context.Context, store db.Storage, binding db.FederationBinding, remoteProjectID int64, client *Client, validateLease func(context.Context) error, turn *relaySyncTurn) error {
	quarantines, err := store.ActiveFederationQuarantinesByProject(ctx, binding.ProjectID)
	if err != nil {
		return err
	}
	if len(quarantines) > 0 {
		if quarantines[0].Direction == db.FederationQuarantineDirectionPull {
			return db.ErrFederationPullQuarantined
		}
		return db.ErrFederationPushQuarantined
	}
	if err := syncRelayPushEvents(ctx, store, binding, remoteProjectID, client, validateLease, turn); err != nil {
		return err
	}
	if err := syncRelayPushArtifacts(ctx, store, binding, remoteProjectID, client, validateLease, turn); err != nil {
		return err
	}
	if err := store.RecordFederationSyncPushSuccess(ctx, binding.ProjectID, time.Now().UTC()); err != nil {
		return err
	}
	return syncRelayPullReceiptsBeforeReset(ctx, store, binding, remoteProjectID, client, validateLease, turn)
}

func syncRelayPushEvents(ctx context.Context, store db.Storage, binding db.FederationBinding, remoteProjectID int64, client *Client, validateLease func(context.Context) error, turn *relaySyncTurn) error {
	c := binding.RelayConfig
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt} {
		for batches := 1; ; batches++ {
			if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
				return err
			}
			pending, err := store.PendingRelayDeliveries(ctx, c.BindingUID, stream, relaySyncBatchLimit)
			if err != nil {
				return err
			}
			if len(pending) == 0 {
				break
			}
			batch := db.RelayBatch{Stream: stream, Envelopes: pending}
			accepted, err := client.AcceptRelayDeliveries(ctx, remoteProjectID, batch)
			if err != nil {
				return recordRelayEventQuarantine(ctx, store, binding, db.FederationQuarantineDirectionPush, batch, err, validateLease)
			}
			if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
				return err
			}
			// Native ACK validation resolves the exact retained emitted endpoint,
			// including a previously emitted prefix after a lost response.
			if err := store.AckRelayDeliveries(ctx, c.BindingUID, c.ResetEpoch, stream, accepted.Through, accepted.Digest); err != nil {
				return err
			}
			if turn.spent(batches) {
				break
			}
		}
	}
	return nil
}

// syncRelayPullReceiptsBeforeReset records root proof for local events already
// accepted upstream without applying event deliveries that depend on the new
// baseline.
func syncRelayPullReceiptsBeforeReset(ctx context.Context, store db.Storage, binding db.FederationBinding, remoteProjectID int64, client *Client, validateLease func(context.Context) error, turn *relaySyncTurn) error {
	c := binding.RelayConfig
	if err := store.RecordFederationSyncPullStarted(ctx, binding.ProjectID, time.Now().UTC()); err != nil {
		return err
	}
	for batches := 1; ; batches++ {
		if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
			return err
		}
		batch, err := client.OfferRelayDeliveries(ctx, remoteProjectID, db.RelayStreamReceipt, relaySyncBatchLimit, c.ResetEpoch)
		if err != nil {
			return err
		}
		if batch.Stream != db.RelayStreamReceipt {
			return errors.New("relay offer uses a different stream")
		}
		if len(batch.Envelopes) == 0 {
			break
		}
		accepted, err := store.AcceptRelayDeliveries(ctx, c.BindingUID, batch)
		if err != nil {
			return recordRelayEventQuarantine(ctx, store, binding, db.FederationQuarantineDirectionPull, batch, err, validateLease)
		}
		if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
			return err
		}
		if err := client.AckRelayDeliveries(ctx, remoteProjectID, c.ResetEpoch, db.RelayStreamReceipt, accepted); err != nil {
			return err
		}
		if turn.spent(batches) {
			break
		}
	}
	return store.RecordFederationSyncPullSuccess(ctx, binding.ProjectID, time.Now().UTC())
}

// Validate the complete replacement path before changing a pin. An authenticated
// hop cannot replace root authority without authorization from the existing key.
func relayRootTransitionPath(pin, advertised db.RootKeyPin, history []db.RootKeyTransition) ([]db.RootKeyTransition, error) {
	return federationcoord.RelayRootTransitionPath(pin, advertised, history)
}

// A rejection from the configured, origin-pinned upstream is an observed loss
// of authority. Persist it under the native policy/binding fence so descendants
// stop serving. Timeouts, server errors and protocol conflicts do not revoke.
// A rejected or unverifiable request signature is a signing configuration
// problem: the grant is intact, so it is reported instead of recorded as
// revocation.
func recordRelayUpstreamRejection(ctx context.Context, store db.Storage, original db.FederationBinding, syncErr error) error {
	status, ok := errors.AsType[*HubStatusError](syncErr)
	if !ok {
		return syncErr
	}
	if hubSignatureRejection(status) {
		return fmt.Errorf("%w: %w", federationsigning.ErrHubRejected, syncErr)
	}
	if status.StatusCode != http.StatusUnauthorized && status.StatusCode != http.StatusForbidden && status.StatusCode != http.StatusNotFound {
		return syncErr
	}
	current, err := store.FederationBindingByProject(ctx, original.ProjectID)
	if err != nil {
		return errors.Join(syncErr, err)
	}
	config := current.RelayConfig
	if config == nil || config.BindingUID != original.RelayConfig.BindingUID || config.ResetEpoch != original.RelayConfig.ResetEpoch || config.UpstreamRevoked {
		return syncErr
	}
	revoked := *config
	revoked.UpstreamRevoked = true
	_, err = store.SetRelayBindingConfig(ctx, current.ProjectID, revoked)
	return errors.Join(syncErr, err)
}

// hubSignatureRejection reports whether the hub refused this daemon's request
// signature or could not verify it, as opposed to refusing the bearer grant.
func hubSignatureRejection(status *HubStatusError) bool {
	var envelope api.ErrorEnvelope
	if json.Unmarshal([]byte(status.Body), &envelope) != nil {
		return false
	}
	return envelope.Error.Code == "signature_invalid" || envelope.Error.Code == "signing_unavailable"
}
