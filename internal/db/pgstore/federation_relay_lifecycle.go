package pgstore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// ValidateRelayLifecycle checks the project’s retained relay state before lifecycle changes.
func (d *Store) ValidateRelayLifecycle(ctx context.Context, projectID int64) error {
	return d.relayTx(ctx, func(tx *sql.Tx) error { return d.relayReset(tx).ValidateLifecycle(ctx, projectID) })
}

// FenceRelayDisconnect validates retained relay work and disables writes in
// one transaction so a concurrent local mutation cannot slip between the two.
func (d *Store) FenceRelayDisconnect(ctx context.Context, projectID int64) error {
	return d.relayTx(ctx, func(tx *sql.Tx) error { return d.relayReset(tx).FenceDisconnect(ctx, projectID) })
}

func (d *Store) validateRelayLifecycleTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	return d.relayReset(tx).ValidateLifecycle(ctx, projectID)
}

func (d *Store) discardRelayHopStateTx(ctx context.Context, tx *sql.Tx, projectID int64, relay *db.RelayBindingConfig) error {
	return d.relayReset(tx).DiscardHopState(ctx, projectID, relay)
}

// deleteRelayBindingStateTx removes one relay binding's outbox, inbox and
// cursor rows. Revocation and detach call it in the transaction that ends the
// binding's authority; live bindings keep their rows.
func deleteRelayBindingStateTx(ctx context.Context, tx *sql.Tx, bindingUID string) error {
	for _, query := range []string{
		`DELETE FROM federation_relay_outbox WHERE binding_uid=$1`,
		`DELETE FROM federation_relay_inbox WHERE binding_uid=$1`,
		`DELETE FROM federation_relay_cursors WHERE binding_uid=$1`,
	} {
		if _, err := tx.ExecContext(ctx, query, bindingUID); err != nil {
			return mapSQLError(err, nil)
		}
	}
	return nil
}

func (b relayResetBackend) DeleteRelayBindingState(ctx context.Context, bindingUID string) error {
	return deleteRelayBindingStateTx(ctx, b.tx, bindingUID)
}

func (b relayResetBackend) ProjectUID(ctx context.Context, projectID int64) (string, error) {
	var projectUID string
	err := b.tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, projectID).Scan(&projectUID)
	return projectUID, err
}

func (b relayResetBackend) RelayBinding(ctx context.Context, projectID int64) (db.FederationBinding, error) {
	return scanFederationBinding(b.tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
}

func (b relayResetBackend) RelayUpstreamDeliveriesOwed(ctx context.Context, projectUID, bindingUID string) (bool, error) {
	var owed bool
	err := b.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_relay_outbox o
WHERE o.project_uid=$1 AND o.binding_uid=$2 AND o.acknowledged=0)`, projectUID, bindingUID).Scan(&owed)
	return owed, err
}

func (b relayResetBackend) RelayLocalEventsAwaitingReceipt(ctx context.Context, projectUID, bindingUID string, epoch int64, originUID string) ([]db.RelayEventRef, error) {
	rows, err := b.tx.QueryContext(ctx, `SELECT o.source_uid,o.source_hash FROM federation_relay_outbox o
WHERE o.project_uid=$1 AND o.binding_uid=$2 AND o.reset_epoch=$3 AND o.stream='events'
  AND o.envelope::jsonb->'path'->>0=$4
  AND NOT EXISTS(SELECT 1 FROM federation_event_provenance r WHERE r.project_uid=o.project_uid AND r.event_uid=o.source_uid AND r.content_hash=o.source_hash)
ORDER BY o.id`, projectUID, bindingUID, epoch, originUID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var refs []db.RelayEventRef
	for rows.Next() {
		var ref db.RelayEventRef
		if err := rows.Scan(&ref.EventUID, &ref.ContentHash); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return refs, rows.Close()
}

func (b relayResetBackend) PendingClaimRequestsOpen(ctx context.Context, projectID int64) (bool, error) {
	var open bool
	err := b.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_claim_requests
WHERE project_id=$1 AND rejected_at IS NULL AND resolved_at IS NULL)`, projectID).Scan(&open)
	return open, err
}

func (b relayResetBackend) RelayDownstreamDeliveriesOwed(ctx context.Context, projectUID string) (bool, error) {
	var owed bool
	err := b.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_relay_outbox o
JOIN federation_enrollments e ON e.relay_binding_uid=o.binding_uid AND e.relay_reset_epoch=o.reset_epoch
WHERE o.project_uid=$1 AND o.acknowledged=0 AND e.revoked_at IS NULL)`, projectUID).Scan(&owed)
	return owed, err
}

func (b relayResetBackend) RelayDownstreamEnrollmentsActive(ctx context.Context, projectID int64) (bool, error) {
	var active bool
	err := b.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_enrollments WHERE project_id=$1 AND relay_protocol_version=1 AND revoked_at IS NULL)`, projectID).Scan(&active)
	if err != nil {
		return false, mapSQLError(err, nil)
	}
	return active, nil
}

func (b relayResetBackend) LockProject(ctx context.Context, projectID int64) error {
	_, err := scanProject(b.tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1 FOR UPDATE`, projectID))
	return err
}

func (b relayResetBackend) RejectIssueSyncedProject(ctx context.Context, projectID int64) error {
	return rejectIssueSyncedFederationProject(ctx, b.tx, projectID)
}

func (b relayResetBackend) RejectExternalRootProject(ctx context.Context, projectID int64) error {
	var bindingID int64
	err := b.tx.QueryRowContext(ctx, `SELECT id FROM external_root_bindings
WHERE project_id=$1 AND active=1 LIMIT 1`, projectID).Scan(&bindingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapSQLError(err, nil)
	}
	return db.ErrExternalRootFederationConflict
}

func (b relayResetBackend) DisableRelayPush(ctx context.Context, projectID int64) error {
	_, err := b.tx.ExecContext(ctx, `UPDATE federation_bindings
SET enabled=1,push_enabled=0,
    updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE project_id=$1 AND role=$2`, projectID, string(db.FederationRoleSpoke))
	if err != nil {
		return mapSQLError(err, nil)
	}
	return nil
}

func (b relayResetBackend) ReconcileBindingTransitionLinks(ctx context.Context, previous *db.FederationBinding, next db.FederationBinding) error {
	return reconcileFederationBindingTransitionLinks(ctx, b.tx, previous, next)
}
