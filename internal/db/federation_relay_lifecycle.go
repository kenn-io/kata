package db

import (
	"context"
	"errors"
	"fmt"
)

// RelayLifecycleStore exposes the same native preflight used at destructive
// commit. Call before external credential teardown; normal pause retains intent.
type RelayLifecycleStore interface {
	ValidateRelayLifecycle(context.Context, int64) error
}

// RelayDisconnectFenceStore validates retained relay work and disables relay
// writes in the same native transaction before upstream revocation.
type RelayDisconnectFenceStore interface {
	FenceRelayDisconnect(context.Context, int64) error
}

// RelayLocalDetacher detaches a relay replica without its upstream hub. It
// discards the binding's undeliverable outbox, inbox and cursor state, keeps
// the project and its issues, and refuses while downstream enrollments remain
// active with ErrFederationResetBlockedByDownstream.
type RelayLocalDetacher interface {
	DetachRelayReplicaLocally(context.Context, int64) (LeaveFederationResult, error)
}

// ValidateLifecycle rechecks durable relay intent in the transaction that
// archives, removes or detaches a project. Those discard the hop, so the
// upstream must hold everything the root has not accepted and every
// descendant must be synchronized and gone first. A project without a
// negotiated relay hop has nothing to validate.
func (p RelayResetProtocol) ValidateLifecycle(ctx context.Context, projectID int64) error {
	projectUID, config, err := p.spokeConfig(ctx, projectID)
	if err != nil || config == nil {
		return err
	}
	if err := p.ValidateUpstreamWork(ctx, projectID, projectUID, config, nil); err != nil {
		return err
	}
	owed, err := p.Backend.RelayDownstreamDeliveriesOwed(ctx, projectUID)
	if err != nil {
		return err
	}
	if owed {
		return ErrFederationResetBlockedByPendingPush
	}
	active, err := p.Backend.RelayDownstreamEnrollmentsActive(ctx, projectID)
	if err != nil {
		return err
	}
	if active {
		return ErrFederationResetBlockedByDownstream
	}
	return nil
}

// spokeConfig loads the negotiated relay configuration of a spoke project. A
// nil config means the project has no relay hop to validate.
func (p RelayResetProtocol) spokeConfig(ctx context.Context, projectID int64) (string, *RelayBindingConfig, error) {
	binding, err := p.Backend.RelayBinding(ctx, projectID)
	if errors.Is(err, ErrNotFound) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if binding.Role != FederationRoleSpoke || binding.RelayConfig == nil {
		return "", nil, nil
	}
	projectUID, err := p.Backend.ProjectUID(ctx, projectID)
	if err != nil {
		return "", nil, err
	}
	if !ProjectAttributionVisible(ctx, projectUID) {
		return "", nil, ErrNotFound
	}
	return projectUID, binding.RelayConfig, nil
}

// ValidateUpstreamWork refuses to discard work the root has not accepted:
// unacknowledged upstream deliveries, hop-acknowledged local events still
// awaiting their root receipt, and unresolved claim requests. A checkpoint
// being installed may attest events in rootAccepted instead.
func (p RelayResetProtocol) ValidateUpstreamWork(ctx context.Context, projectID int64, projectUID string, config *RelayBindingConfig, rootAccepted map[string]string) error {
	owed, err := p.Backend.RelayUpstreamDeliveriesOwed(ctx, projectUID, config.BindingUID)
	if err != nil {
		return err
	}
	if owed {
		return ErrFederationResetBlockedByPendingPush
	}
	awaiting, err := p.Backend.RelayLocalEventsAwaitingReceipt(ctx, projectUID, config.BindingUID, config.ResetEpoch, p.InstanceUID)
	if err != nil {
		return err
	}
	if err := RelayLocalWorkBlocksReset(awaiting, rootAccepted); err != nil {
		return err
	}
	open, err := p.Backend.PendingClaimRequestsOpen(ctx, projectID)
	if err != nil {
		return err
	}
	if open {
		return ErrFederationResetBlockedByPendingPush
	}
	return nil
}

// RelayLocalWorkBlocksReset decides whether local events the hop acknowledged
// but the root has not receipted block discarding the hop. The checkpoint
// being installed may attest an event through rootAccepted, keyed by event
// UID with the accepted content hash; any other awaiting event blocks with
// ErrFederationResetBlockedByPendingPush.
func RelayLocalWorkBlocksReset(awaiting []RelayEventRef, rootAccepted map[string]string) error {
	for _, event := range awaiting {
		if rootAccepted[event.EventUID] != event.ContentHash {
			return ErrFederationResetBlockedByPendingPush
		}
	}
	return nil
}

// DiscardHopState removes one relay binding's per-hop state after checking
// that no active downstream enrollment still depends on it.
func (p RelayResetProtocol) DiscardHopState(ctx context.Context, projectID int64, relay *RelayBindingConfig) error {
	active, err := p.Backend.RelayDownstreamEnrollmentsActive(ctx, projectID)
	if err != nil {
		return fmt.Errorf("check downstream relay enrollments: %w", err)
	}
	if active {
		return ErrFederationResetBlockedByDownstream
	}
	if relay == nil {
		return nil
	}
	return p.Backend.DeleteRelayBindingState(ctx, relay.BindingUID)
}

// FenceDisconnect validates retained relay work and disables relay writes in
// one transaction so a concurrent local mutation cannot slip between the two.
// A project without a binding has nothing to fence.
func (p RelayResetProtocol) FenceDisconnect(ctx context.Context, projectID int64) error {
	if err := p.Backend.LockProject(ctx, projectID); err != nil {
		return err
	}
	if err := p.ValidateLifecycle(ctx, projectID); err != nil {
		return err
	}
	binding, err := p.Backend.RelayBinding(ctx, projectID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if binding.Role != FederationRoleSpoke {
		return ErrFederationRebindConflict
	}
	next := binding
	next.Enabled = true
	next.PushEnabled = false
	if err := CheckRelayBindingUpdate(&binding, next); err != nil {
		return err
	}
	if err := p.Backend.RejectIssueSyncedProject(ctx, projectID); err != nil {
		return err
	}
	if err := p.Backend.RejectExternalRootProject(ctx, projectID); err != nil {
		return err
	}
	if err := p.Backend.DisableRelayPush(ctx, projectID); err != nil {
		return err
	}
	return p.Backend.ReconcileBindingTransitionLinks(ctx, &binding, next)
}
