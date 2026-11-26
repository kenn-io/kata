package sqlitestore

import (
	"context"
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
	return d.UpdatePriorityGuarded(ctx, db.UpdatePriorityParams{
		IssueID: issueID, Priority: newPriority, Actor: actor,
	})
}

// UpdatePriorityGuarded sets issues.priority with an optional revision guard.
func (d *Store) UpdatePriorityGuarded(ctx context.Context, p db.UpdatePriorityParams) (db.Issue, *db.Event, bool, error) {
	return retryWrite3(ctx, d, func() (db.Issue, *db.Event, bool, error) {
		return d.updatePriority(ctx, p)
	})
}

func (d *Store) updatePriority(ctx context.Context, p db.UpdatePriorityParams) (db.Issue, *db.Event, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, p.IssueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if p.IfMatchRev != nil && *p.IfMatchRev != issue.Revision {
		return db.Issue{}, nil, false, &db.RevisionConflictError{CurrentRevision: issue.Revision}
	}
	if priorityEqual(issue.Priority, p.Priority) {
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		return issue, nil, false, nil
	}

	ts := nowTimestamp()
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues
		 SET priority   = ?,
		     revision   = revision + 1,
		     updated_at = ?
		 WHERE id = ?`, p.Priority, ts, p.IssueID); err != nil {
		return db.Issue{}, nil, false, fmt.Errorf("update priority: %w", err)
	}

	eventType, payload, err := priorityEventPayload(issue.Priority, p.Priority, ts)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   issue.ProjectID,
		ProjectName: projectName,
		IssueID:     &issue.ID,
		Type:        eventType,
		Actor:       p.Actor,
		Payload:     payload,
	})
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	updated, err := issueByIDTx(ctx, tx, p.IssueID)
	if err != nil {
		return db.Issue{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
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
	return db.PriorityEventPayload(old, newPrio, ts)
}
