package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

// Retain both live-epoch deliveries and prepared resets until activation.
func rejectPendingRelayResetTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, project db.Project) error {
	var pendingRelay bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_relay_outbox o JOIN federation_enrollments e ON e.relay_binding_uid=o.binding_uid AND e.relay_reset_epoch=o.reset_epoch WHERE o.project_uid=$1 AND o.acknowledged=0 AND e.revoked_at IS NULL) OR EXISTS(SELECT 1 FROM federation_enrollments e JOIN meta m ON m.key=$2 || e.relay_binding_uid WHERE e.project_id=$3 AND e.revoked_at IS NULL AND (m.value::jsonb->'translation'->'authority'->>'epoch')::bigint>e.relay_reset_epoch)`, project.UID, db.RelayResetMetadataPrefix+project.UID+".", project.ID).Scan(&pendingRelay); err != nil {
		return err
	}
	if pendingRelay {
		return db.ErrFederationResetBlockedByPendingPush
	}
	return nil
}
