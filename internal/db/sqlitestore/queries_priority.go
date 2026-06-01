package sqlitestore

import (
	"context"
	"encoding/json"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// UpdatePriority sets issues.priority to the new value and emits the matching
// priority_set / priority_cleared event. newPriority == nil means clear. No-op
// when the new value matches the current value (returns nil event,
// changed=false).
//
// Event payloads:
//   - issue.priority_set:     {"priority": <new>, "old_priority": <old>}
//     where old_priority is omitted when the prior value was nil.
//   - issue.priority_cleared: {"old_priority": <old>}
//     emitted only when there was a prior value to clear; clearing an
//     already-null priority is a no-op (changed=false, no event).
func (d *Store) UpdatePriority(ctx context.Context, issueID int64, newPriority *int64, actor string) (db.Issue, *db.Event, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if priorityEqual(issue.Priority, newPriority) {
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		return issue, nil, false, nil
	}

	ts := nowTimestamp()
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues
		 SET priority   = ?,
		     updated_at = ?
		 WHERE id = ?`, newPriority, ts, issueID); err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("update priority: %w", err)
	}

	eventType, payload, err := priorityEventPayload(issue.Priority, newPriority, ts)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        eventType,
		Actor:       actor,
		Payload:     payload,
	})
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return db.Issue{}, nil, false, err
	}
	updated, err := d.IssueByID(ctx, issueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	return updated, &evt, true, nil
}

func priorityEqual(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// priorityEventPayload returns the event type and JSON payload for a
// priority transition from old to new. old==new is rejected as a programming
// error because UpdatePriority short-circuits no-ops before reaching here.
func priorityEventPayload(old, newPrio *int64, ts string) (string, string, error) {
	type setPayload struct {
		Priority    int64  `json:"priority"`
		OldPriority *int64 `json:"old_priority,omitempty"`
		UpdatedAt   string `json:"updated_at"`
	}
	type clearedPayload struct {
		OldPriority int64  `json:"old_priority"`
		UpdatedAt   string `json:"updated_at"`
	}
	if newPrio != nil {
		bs, err := json.Marshal(setPayload{Priority: *newPrio, OldPriority: old, UpdatedAt: ts})
		if err != nil {
			return "", "", fmt.Errorf("marshal priority_set payload: %w", err)
		}
		return "issue.priority_set", string(bs), nil
	}
	// Clearing: old must be non-nil (priorityEqual short-circuits two nils).
	if old == nil {
		return "", "", fmt.Errorf("priorityEventPayload: cannot clear a nil priority")
	}
	bs, err := json.Marshal(clearedPayload{OldPriority: *old, UpdatedAt: ts})
	if err != nil {
		return "", "", fmt.Errorf("marshal priority_cleared payload: %w", err)
	}
	return "issue.priority_cleared", string(bs), nil
}
