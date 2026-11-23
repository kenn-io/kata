package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"slices"
)

// NeedsReviewContext records closes carried by an issue snapshot.
type NeedsReviewContext struct {
	Closes []string `json:"closes"`
}

func sortedUnique(values []string) []string {
	values = append([]string{}, values...)
	slices.Sort(values)
	return slices.Compact(values)
}

// NeedsReviewContextTx folds an issue's stored history to carry its close context
// into a new label event or snapshot.
func NeedsReviewContextTx(ctx context.Context, tx *sql.Tx, issueUID, placeholder string) (NeedsReviewContext, error) {
	projection, err := needsReviewProjectionTx(ctx, tx, issueUID, placeholder)
	if err != nil {
		return NeedsReviewContext{}, err
	}
	return projection.NeedsReviewContexts[issueUID], nil
}

// NeedsReviewRepairTx reports whether an older peer's label was cleared by a close.
func NeedsReviewRepairTx(ctx context.Context, tx *sql.Tx, event RemoteEvent, placeholder string) (bool, error) {
	if (event.Type != "issue.labeled" && event.Type != "issue.closed") || event.IssueUID == nil {
		return false, nil
	}
	if event.Type == "issue.labeled" {
		payload := PayloadMap(event.Payload)
		label, _ := StringValue(payload["label"])
		if _, ok := payload["after_close_uids"]; label != "needs-review" || ok {
			return false, nil
		}
	}
	projection, err := needsReviewProjectionTx(ctx, tx, *event.IssueUID, placeholder)
	if err != nil {
		return false, err
	}
	state := projection.Labels[FoldLabelKey{IssueUID: *event.IssueUID, Label: "needs-review"}]
	if state.Present || state.Clock.EventUID == "" || (event.Type == "issue.labeled" && state.Clock.EventUID != event.EventUID) {
		return false, nil
	}
	var typ, raw string
	if err := tx.QueryRowContext(ctx, `SELECT type, payload FROM events WHERE uid = `+placeholder, state.Clock.EventUID).Scan(&typ, &raw); err != nil {
		return false, err
	}
	payload := PayloadMap(jsontext.Value(raw))
	_, hasContext := payload["after_close_uids"]
	return typ == "issue.labeled" && !hasContext, nil
}

func needsReviewProjectionTx(ctx context.Context, tx *sql.Tx, issueUID, placeholder string) (FoldProjection, error) {
	query := `SELECT uid, origin_instance_uid, type, payload, hlc_physical_ms, hlc_counter FROM events WHERE issue_uid = ` + placeholder + ` AND type IN ('issue.created','issue.snapshot','issue.moved','issue.closed','issue.labeled','issue.unlabeled', '` + RepeatedImportEventType + `')`
	rows, err := tx.QueryContext(ctx, query, issueUID)
	if err != nil {
		return FoldProjection{}, fmt.Errorf("read needs-review history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var events []FoldEvent
	for rows.Next() {
		var e FoldEvent
		var payload string
		if err := rows.Scan(&e.UID, &e.OriginInstanceUID, &e.Type, &payload, &e.HLCPhysicalMS, &e.HLCCounter); err != nil {
			return FoldProjection{}, err
		}
		e.IssueUID, e.Payload = issueUID, jsontext.Value(payload)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return FoldProjection{}, err
	}
	return FoldEvents(events), nil
}
