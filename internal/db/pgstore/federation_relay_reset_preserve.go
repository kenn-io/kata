package pgstore

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

func (b relayResetBackend) RelayPostCheckpointReceipts(ctx context.Context, projectID int64, projectUID string, epoch, sequence int64) ([]db.RelayRetainedReceiptRow, error) {
	rows, err := b.tx.QueryContext(ctx, `SELECT e.id,p.receipt,p.content_hash,p.reset_epoch,p.sequence,p.key_id
FROM events e JOIN federation_event_provenance p ON p.project_uid=$1 AND p.event_uid=e.uid
WHERE e.project_id=$2 AND (p.reset_epoch>$3 OR (p.reset_epoch=$4 AND p.sequence>$5))
ORDER BY p.reset_epoch,p.sequence,e.uid`, projectUID, projectID, epoch, epoch, sequence)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var retained []db.RelayRetainedReceiptRow
	for rows.Next() {
		var row db.RelayRetainedReceiptRow
		if err := rows.Scan(&row.EventID, &row.Receipt, &row.ContentHash, &row.ResetEpoch, &row.Sequence, &row.KeyID); err != nil {
			return nil, err
		}
		retained = append(retained, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return retained, rows.Close()
}

func (b relayResetBackend) StoredEvent(ctx context.Context, eventID int64) (db.Event, error) {
	return scanEvent(b.tx.QueryRowContext(ctx, eventSelect+` WHERE e.id=$1`, eventID))
}
