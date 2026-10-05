package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// ValidateRelayLifecycle checks the project’s retained relay state before lifecycle changes.
func (d *Store) ValidateRelayLifecycle(ctx context.Context, projectID int64) error {
	return d.relayTx(ctx, func(tx *sql.Tx) error { return d.validateRelayLifecycleTx(ctx, tx, projectID) })
}

// The binding lock serializes descendant enrollment with teardown. Recheck
// durable intent in the transaction performing archive, detach or reset.
func (d *Store) validateRelayLifecycleTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=?`, projectID))
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if binding.Role != db.FederationRoleSpoke || binding.RelayConfig == nil {
		return nil
	}
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=?`, projectID).Scan(&projectUID); err != nil {
		return err
	}
	if !db.ProjectAttributionVisible(ctx, projectUID) {
		return db.ErrNotFound
	}
	config := binding.RelayConfig
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_relay_outbox o
WHERE o.project_uid=? AND ((o.acknowledged=0 AND (o.binding_uid=? OR EXISTS(SELECT 1 FROM federation_enrollments e WHERE e.relay_binding_uid=o.binding_uid AND e.relay_reset_epoch=o.reset_epoch AND e.revoked_at IS NULL))) OR (o.binding_uid=? AND o.reset_epoch=? AND o.stream='events' AND json_extract(o.envelope,'$.path[0]')=? AND NOT EXISTS(SELECT 1 FROM federation_event_provenance r WHERE r.project_uid=o.project_uid AND r.event_uid=o.source_uid AND r.content_hash=o.source_hash))))`, projectUID, config.BindingUID, config.BindingUID, config.ResetEpoch, d.instanceUID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return db.ErrFederationResetBlockedByPendingPush
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_claim_requests WHERE project_id=?)`, projectID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return db.ErrFederationResetBlockedByPendingPush
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_enrollments WHERE project_id=? AND relay_protocol_version=1 AND revoked_at IS NULL)`, projectID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return db.ErrFederationResetBlockedByDownstream
	}
	return nil
}
