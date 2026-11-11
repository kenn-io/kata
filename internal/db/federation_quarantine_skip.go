package db

import (
	"context"
	"database/sql"
)

// SkipFederationPushBatch retains the skipped events without passing earlier pending work.
func SkipFederationPushBatch(ctx context.Context, tx *sql.Tx, q FederationQuarantine, origin string) (int64, error) {
	if len(q.EventUIDs) == 0 {
		return q.LastEventID, nil
	}
	for _, uid := range q.EventUIDs {
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_push_accepted(project_id, event_id)
SELECT project_id, id FROM events WHERE project_id=$1 AND origin_instance_uid=$2 AND uid=$3
ON CONFLICT(project_id, event_id) DO NOTHING`, q.ProjectID, origin, uid)
		if err != nil {
			return 0, err
		}
	}
	var first sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT MIN(e.id) FROM events e JOIN federation_bindings b ON b.project_id=e.project_id
WHERE e.project_id=$1 AND e.origin_instance_uid=$2 AND e.id>b.push_cursor_event_id AND e.id<=$3
AND e.type IN (`+FederationPushEventTypesSQL()+`)
AND NOT EXISTS (SELECT 1 FROM federation_push_accepted a WHERE a.project_id=e.project_id AND a.event_id=e.id)`, q.ProjectID, origin, q.LastEventID).Scan(&first)
	if err != nil {
		return 0, err
	}
	if first.Valid {
		return first.Int64 - 1, nil
	}
	return q.LastEventID, nil
}
