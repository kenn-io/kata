package pgstore

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

// FenceRelayDisconnect validates retained relay work and disables writes in
// one transaction so a concurrent local mutation cannot slip between the two.
func (d *Store) FenceRelayDisconnect(ctx context.Context, projectID int64) error {
	return d.relayTx(ctx, func(tx *sql.Tx) error {
		if _, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id=$1 FOR UPDATE`, projectID)); err != nil {
			return err
		}
		if err := d.validateRelayLifecycleTx(ctx, tx, projectID); err != nil {
			return err
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
			federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if binding.Role != db.FederationRoleSpoke {
			return db.ErrFederationRebindConflict
		}
		next := binding
		next.Enabled = true
		next.PushEnabled = false
		if err := db.CheckRelayBindingUpdate(&binding, next); err != nil {
			return err
		}
		var issueSyncProjectID int64
		err = tx.QueryRowContext(ctx, `SELECT project_id FROM issue_sync_bindings
WHERE project_id=$1 AND enabled=1`, projectID).Scan(&issueSyncProjectID)
		if err == nil {
			return db.ErrIssueSyncFederationBinding
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return mapSQLError(err, nil)
		}
		var externalRootBindingID int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM external_root_bindings
WHERE project_id=$1 AND active=1 LIMIT 1`, projectID).Scan(&externalRootBindingID)
		if err == nil {
			return db.ErrExternalRootFederationConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return mapSQLError(err, nil)
		}
		_, err = tx.ExecContext(ctx, `UPDATE federation_bindings
SET enabled=1,push_enabled=0,
    updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE project_id=$1 AND role=$2`, projectID, string(db.FederationRoleSpoke))
		if err != nil {
			return mapSQLError(err, nil)
		}
		return reconcileFederationBindingTransitionLinks(ctx, tx, &binding, next)
	})
}

// The binding lock serializes descendant enrollment with teardown. Recheck
// durable intent in the transaction performing archive, detach or reset.
func (d *Store) validateRelayLifecycleTx(ctx context.Context, tx *sql.Tx, projectID int64, checkpointAcceptedEvents ...map[string]string) error {
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
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
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, projectID).Scan(&projectUID); err != nil {
		return err
	}
	if !db.ProjectAttributionVisible(ctx, projectUID) {
		return db.ErrNotFound
	}
	config := binding.RelayConfig
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_relay_outbox o
WHERE o.project_uid=$1 AND o.acknowledged=0 AND (o.binding_uid=$2 OR EXISTS(SELECT 1 FROM federation_enrollments e WHERE e.relay_binding_uid=o.binding_uid AND e.relay_reset_epoch=o.reset_epoch AND e.revoked_at IS NULL)))`, projectUID, config.BindingUID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return db.ErrFederationResetBlockedByPendingPush
	}
	var rootAccepted map[string]string
	if len(checkpointAcceptedEvents) > 0 {
		rootAccepted = checkpointAcceptedEvents[0]
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.source_uid,o.source_hash FROM federation_relay_outbox o
WHERE o.project_uid=$1 AND o.binding_uid=$2 AND o.reset_epoch=$3 AND o.stream='events'
  AND o.envelope::jsonb->'path'->>0=$4
  AND NOT EXISTS(SELECT 1 FROM federation_event_provenance r WHERE r.project_uid=o.project_uid AND r.event_uid=o.source_uid AND r.content_hash=o.source_hash)
ORDER BY o.id`, projectUID, config.BindingUID, config.ResetEpoch, d.instanceUID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var eventUID, contentHash string
		if err := rows.Scan(&eventUID, &contentHash); err != nil {
			_ = rows.Close()
			return err
		}
		if rootAccepted[eventUID] != contentHash {
			_ = rows.Close()
			return db.ErrFederationResetBlockedByPendingPush
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_claim_requests WHERE project_id=$1)`, projectID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return db.ErrFederationResetBlockedByPendingPush
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_enrollments WHERE project_id=$1 AND relay_protocol_version=1 AND revoked_at IS NULL)`, projectID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return db.ErrFederationResetBlockedByDownstream
	}
	return nil
}
