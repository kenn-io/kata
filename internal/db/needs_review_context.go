package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"slices"
)

// NeedsReviewContext records closes and the closes observed by the label's writer.
type NeedsReviewContext struct {
	Closes []string `json:"closes"`
	Seen   []string `json:"seen"`
}

func sortedUnique(values []string) []string {
	values = append([]string{}, values...)
	slices.Sort(values)
	return slices.Compact(values)
}

// NeedsReviewContextTx folds an issue's stored history to carry its close context
// into a new label event or snapshot.
func NeedsReviewContextTx(ctx context.Context, tx *sql.Tx, issueUID, placeholder string) (NeedsReviewContext, error) {
	query := `SELECT uid, origin_instance_uid, type, payload, hlc_physical_ms, hlc_counter FROM events WHERE issue_uid = ` + placeholder + ` AND type IN ('issue.created','issue.snapshot','issue.moved','issue.closed','issue.labeled','issue.unlabeled', '` + RepeatedImportEventType + `')`
	rows, err := tx.QueryContext(ctx, query, issueUID)
	if err != nil {
		return NeedsReviewContext{}, fmt.Errorf("read needs-review history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var events []FoldEvent
	for rows.Next() {
		var e FoldEvent
		var payload string
		if err := rows.Scan(&e.UID, &e.OriginInstanceUID, &e.Type, &payload, &e.HLCPhysicalMS, &e.HLCCounter); err != nil {
			return NeedsReviewContext{}, err
		}
		e.IssueUID, e.Payload = issueUID, jsontext.Value(payload)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return NeedsReviewContext{}, err
	}
	return FoldEvents(events).NeedsReviewContexts[issueUID], nil
}
