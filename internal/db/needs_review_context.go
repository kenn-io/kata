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

// NeedsReviewRepairTx reports whether a close cleared an older peer's label.
func NeedsReviewRepairTx(ctx context.Context, tx *sql.Tx, event RemoteEvent, placeholder string) (bool, error) {
	if event.IssueUID == nil || (event.Type != "issue.closed" && !isLegacyNeedsReviewAdd(event.Type, event.Payload)) {
		return false, nil
	}
	return legacyNeedsReviewClearedTx(ctx, tx, *event.IssueUID, placeholder)
}

// legacyNeedsReviewClearedTx reports whether the fold dropped needs-review
// because a close outranks a plain add that carries no close context.
func legacyNeedsReviewClearedTx(ctx context.Context, tx *sql.Tx, issueUID, placeholder string) (bool, error) {
	projection, err := needsReviewProjectionTx(ctx, tx, issueUID, placeholder)
	if err != nil {
		return false, err
	}
	state := projection.Labels[FoldLabelKey{IssueUID: issueUID, Label: "needs-review"}]
	if state.Present || state.Clock.EventUID == "" {
		return false, nil
	}
	var typ, raw string
	if err := tx.QueryRowContext(ctx, `SELECT type, payload FROM events WHERE uid = `+placeholder, state.Clock.EventUID).Scan(&typ, &raw); err != nil {
		return false, err
	}
	return isLegacyNeedsReviewAdd(typ, jsontext.Value(raw)), nil
}

// isLegacyNeedsReviewAdd reports whether an event adds needs-review without close context.
func isLegacyNeedsReviewAdd(typ string, raw jsontext.Value) bool {
	payload := PayloadMap(raw)
	label, _ := StringValue(payload["label"])
	_, hasContext := payload["after_close_uids"]
	return typ == "issue.labeled" && label == "needs-review" && !hasContext
}

func needsReviewProjectionTx(ctx context.Context, tx *sql.Tx, issueUID, placeholder string) (FoldProjection, error) {
	query := `SELECT uid, origin_instance_uid, type, payload, hlc_physical_ms, hlc_counter FROM events WHERE issue_uid = ` + placeholder + ` AND type IN ('issue.created','issue.snapshot','issue.moved','issue.closed','issue.reopened','issue.labeled','issue.unlabeled', '` + RepeatedImportEventType + `')`
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
