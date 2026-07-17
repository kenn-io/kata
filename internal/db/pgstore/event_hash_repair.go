package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

func collectEventIDs(rows *sql.Rows) ([]int64, error) {
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, mapSQLError(err, nil)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	return ids, nil
}

// recomputeEventContentHashesTx repairs retained envelopes whose canonical
// project or issue UID inputs changed in the same transaction.
func recomputeEventContentHashesTx(ctx context.Context, tx *sql.Tx, ids []int64) error {
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		var input db.EventHashInput
		var payload string
		err := tx.QueryRowContext(ctx, `
SELECT e.uid, e.origin_instance_uid, p.uid, e.project_name,
       e.issue_uid, e.related_issue_uid, e.type, e.actor,
       e.hlc_physical_ms, e.hlc_counter, e.created_at, e.payload
  FROM events e
  JOIN projects p ON p.id = e.project_id
 WHERE e.id = $1`, id).Scan(
			&input.UID, &input.OriginInstanceUID, &input.ProjectUID, &input.ProjectName,
			&input.IssueUID, &input.RelatedIssueUID, &input.Type, &input.Actor,
			&input.HLCPhysicalMS, &input.HLCCounter, &input.CreatedAt, &payload,
		)
		if err != nil {
			return mapSQLError(err, nil)
		}
		input.Payload = json.RawMessage(payload)
		hash, err := db.EventContentHash(input)
		if err != nil {
			return fmt.Errorf("rehash retained event %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE events SET content_hash = $1 WHERE id = $2`, hash, id); err != nil {
			return mapSQLError(err, nil)
		}
	}
	return nil
}
