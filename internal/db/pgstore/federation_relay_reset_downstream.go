package pgstore

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

func (b relayResetBackend) RelayActiveDownstreamGrants(ctx context.Context, projectID int64) ([]db.FederationEnrollment, error) {
	rows, err := b.tx.QueryContext(ctx, federationEnrollmentSelect+` WHERE project_id=$1 AND relay_protocol_version=1 AND revoked_at IS NULL ORDER BY id`, projectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var grants []db.FederationEnrollment
	for rows.Next() {
		grant, err := scanFederationEnrollment(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return grants, rows.Close()
}

func (b relayResetBackend) RelayUnacknowledgedDeliveries(ctx context.Context, bindingUID string, epoch int64) ([]db.RelayDeliveryRow, error) {
	rows, err := b.tx.QueryContext(ctx, `SELECT o.stream,o.source_uid,o.source_hash,
  EXISTS(SELECT 1 FROM federation_event_provenance p WHERE p.project_uid=o.project_uid AND p.event_uid=o.source_uid AND p.content_hash=o.source_hash)
FROM federation_relay_outbox o
WHERE o.binding_uid=$1 AND o.reset_epoch=$2 AND o.acknowledged=0
ORDER BY o.id`, bindingUID, epoch)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var deliveries []db.RelayDeliveryRow
	for rows.Next() {
		var delivery db.RelayDeliveryRow
		if err := rows.Scan(&delivery.Stream, &delivery.SourceUID, &delivery.SourceHash, &delivery.HasReceipt); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return deliveries, rows.Close()
}

func (b relayResetBackend) RelayOutboxEpochSummary(ctx context.Context, bindingUID string, epoch int64) (db.RelayOutboxEpochSummary, error) {
	var summary db.RelayOutboxEpochSummary
	err := b.tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0),
 COALESCE(SUM(CASE WHEN acknowledged=0 AND stream=$1 THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN acknowledged=0 AND stream=$2 THEN 1 ELSE 0 END),0)
FROM federation_relay_outbox WHERE binding_uid=$3 AND reset_epoch=$4`, db.RelayStreamEvent, db.RelayStreamArtifact, bindingUID, epoch).Scan(&summary.ThroughID, &summary.UnacknowledgedEvents, &summary.UnacknowledgedArtifacts)
	if err != nil {
		return db.RelayOutboxEpochSummary{}, mapSQLError(err, nil)
	}
	return summary, nil
}

func (b relayResetBackend) DeleteUnemittedRelayOutbox(ctx context.Context, bindingUID string, epoch int64) error {
	_, err := b.tx.ExecContext(ctx, `DELETE FROM federation_relay_outbox WHERE binding_uid=$1 AND reset_epoch=$2 AND emitted=0`, bindingUID, epoch)
	if err != nil {
		return mapSQLError(err, nil)
	}
	return nil
}
