package db

import (
	"context"
	"database/sql"
)

// FederationMoveSourceObserved distinguishes a concurrent move from a missing
// predecessor. A source already present in this issue's retained ownership
// history needs no new predecessor delivery; deterministic fold clocks decide
// its competing destination. Current-owner authorization is checked separately.
func FederationMoveSourceObserved(ctx context.Context, tx *sql.Tx, issueUID, sourceProjectUID string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.uid,e.type,e.payload FROM events e
 JOIN projects p ON p.id=e.project_id
 WHERE e.issue_uid=$1 AND e.type IN ('issue.created','issue.snapshot','issue.moved')`, issueUID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var projectUID, eventType, payload string
		if err := rows.Scan(&projectUID, &eventType, &payload); err != nil {
			return false, err
		}
		if eventType != "issue.moved" {
			if projectUID == sourceProjectUID {
				return true, nil
			}
			continue
		}
		move := PayloadMap([]byte(payload))
		from, _ := StringValue(move["from_project_uid"])
		to, _ := StringValue(move["to_project_uid"])
		if from == sourceProjectUID || to == sourceProjectUID {
			return true, nil
		}
	}
	return false, rows.Err()
}
