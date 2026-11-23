package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"

	"go.kenn.io/kata/internal/db"
)

func (d *Store) repairLegacyNeedsReviewTx(ctx context.Context, tx *sql.Tx, projectID int64, remote db.RemoteEvent) (*db.Event, error) {
	needed, err := db.NeedsReviewRepairTx(ctx, tx, remote, "?")
	if err != nil || !needed {
		return nil, err
	}
	payload, err := json.Marshal(map[string]string{"issue_uid": *remote.IssueUID, "label": "needs-review", "updated_at": nowTimestamp()})
	if err != nil {
		return nil, err
	}
	event, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: projectID, IssueUID: remote.IssueUID, Type: "issue.unlabeled", Actor: remote.Actor, Payload: string(payload)})
	return &event, err
}
