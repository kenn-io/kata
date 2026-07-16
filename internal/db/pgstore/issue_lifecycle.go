package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// EditIssue changes the requested scalar fields and emits one replayable event.
func (s *Store) EditIssue(ctx context.Context, params db.EditIssueParams) (db.Issue, *db.Event, bool, error) {
	if params.Title == nil && params.Body == nil && params.Owner == nil {
		return db.Issue{}, nil, false, db.ErrNoFields
	}
	var issue db.Issue
	var event *db.Event
	var changed bool
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		issue, event, changed = db.Issue{}, nil, false
		current, project, err := lockedIssueTx(ctx, tx, params.IssueID, false)
		if err != nil {
			return err
		}
		sets := make([]string, 0, 5)
		args := make([]any, 0, 6)
		payload := make(map[string]any)
		if params.Title != nil && *params.Title != current.Title {
			args = append(args, *params.Title)
			sets = append(sets, fmt.Sprintf("title = $%d", len(args)))
			payload["title"], payload["old_title"] = *params.Title, current.Title
		}
		if params.Body != nil && *params.Body != current.Body {
			args = append(args, *params.Body)
			sets = append(sets, fmt.Sprintf("body = $%d", len(args)))
			payload["body"] = *params.Body
		}
		if params.Owner != nil {
			var next *string
			if *params.Owner != "" {
				value := *params.Owner
				next = &value
			}
			if !equalStringPointers(current.Owner, next) {
				args = append(args, next)
				sets = append(sets, fmt.Sprintf("owner = $%d", len(args)))
				payload["owner"], payload["old_owner"] = next, current.Owner
			}
		}
		if len(sets) == 0 {
			issue = current
			return nil
		}
		updatedAt := mutationTimestamp()
		args = append(args, updatedAt)
		sets = append(sets, fmt.Sprintf("updated_at = $%d", len(args)))
		if (params.Title != nil && *params.Title != current.Title) || (params.Body != nil && *params.Body != current.Body) {
			sets = append(sets, "content_revision = content_revision + 1")
		}
		args = append(args, current.ID)
		// Every SET fragment above is a fixed storage-owned literal; caller
		// values remain positional parameters.
		query := `UPDATE issues SET ` + strings.Join(sets, ", ") + fmt.Sprintf(` WHERE id = $%d`, len(args)) // #nosec G202
		if _, err := tx.ExecContext(ctx,
			query, args...); err != nil {
			return mapSQLError(err, nil)
		}
		payload["updated_at"] = updatedAt
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		created, err := s.insertEventTx(ctx, tx, issueEventInput(current, project, "issue.updated", params.Actor, string(body)))
		if err != nil {
			return err
		}
		event, changed = &created, true
		issue, err = scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, current.ID))
		return err
	})
	return issue, event, changed, err
}

// UpdateOwner changes assignment state, treating an equal value as a no-op.
func (s *Store) UpdateOwner(ctx context.Context, issueID int64, owner *string, actor string) (db.Issue, *db.Event, bool, error) {
	return s.updateIssueAttribute(ctx, issueID, actor, func(current db.Issue, updatedAt string) (string, any, string, string, bool, error) {
		if equalStringPointers(current.Owner, owner) {
			return "", nil, "", "", false, nil
		}
		payload := map[string]any{"owner": owner, "updated_at": updatedAt}
		eventType := "issue.unassigned"
		if owner != nil {
			eventType = "issue.assigned"
		}
		body, err := json.Marshal(payload)
		return "owner", owner, eventType, string(body), true, err
	})
}

// UpdatePriority changes priority state, treating an equal value as a no-op.
func (s *Store) UpdatePriority(ctx context.Context, issueID int64, priority *int64, actor string) (db.Issue, *db.Event, bool, error) {
	return s.updateIssueAttribute(ctx, issueID, actor, func(current db.Issue, updatedAt string) (string, any, string, string, bool, error) {
		if equalIntPointers(current.Priority, priority) {
			return "", nil, "", "", false, nil
		}
		payload := map[string]any{"updated_at": updatedAt}
		eventType := "issue.priority_cleared"
		if current.Priority != nil {
			payload["old_priority"] = *current.Priority
		}
		if priority != nil {
			payload["priority"] = *priority
			eventType = "issue.priority_set"
		}
		body, err := json.Marshal(payload)
		return "priority", priority, eventType, string(body), true, err
	})
}

type issueAttributePlan func(db.Issue, string) (
	column string,
	value any,
	eventType string,
	payload string,
	changed bool,
	err error,
)

func (s *Store) updateIssueAttribute(
	ctx context.Context,
	issueID int64,
	actor string,
	plan issueAttributePlan,
) (db.Issue, *db.Event, bool, error) {
	var issue db.Issue
	var event *db.Event
	var changed bool
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		issue, event, changed = db.Issue{}, nil, false
		current, project, err := lockedIssueTx(ctx, tx, issueID, false)
		if err != nil {
			return err
		}
		updatedAt := mutationTimestamp()
		column, value, eventType, payload, shouldChange, err := plan(current, updatedAt)
		if err != nil {
			return err
		}
		if !shouldChange {
			issue = current
			return nil
		}
		var query string
		switch column {
		case "owner":
			query = `UPDATE issues SET owner = $1, updated_at = $2 WHERE id = $3`
		case "priority":
			query = `UPDATE issues SET priority = $1, updated_at = $2 WHERE id = $3`
		default:
			return fmt.Errorf("unsupported issue attribute %q", column)
		}
		if _, err := tx.ExecContext(ctx, query, value, updatedAt, current.ID); err != nil {
			return mapSQLError(err, nil)
		}
		created, err := s.insertEventTx(ctx, tx, issueEventInput(current, project, eventType, actor, payload))
		if err != nil {
			return err
		}
		event, changed = &created, true
		issue, err = scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, current.ID))
		return err
	})
	return issue, event, changed, err
}

// ClaimOwner atomically assigns an unowned issue or force-replaces its owner.
func (s *Store) ClaimOwner(ctx context.Context, issueID int64, actor string, force bool) (db.ClaimResult, error) {
	actor = strings.TrimSpace(actor)
	var result db.ClaimResult
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		result = db.ClaimResult{}
		current, project, err := lockedIssueTx(ctx, tx, issueID, false)
		if err != nil {
			return err
		}
		if current.Owner != nil && *current.Owner == actor {
			result.Issue = current
			return nil
		}
		if current.Owner != nil && !force {
			result.CurrentOwner = current.Owner
			return db.ErrAlreadyClaimed
		}
		result.PreviousOwner = current.Owner
		updatedAt := mutationTimestamp()
		if _, err := tx.ExecContext(ctx,
			`UPDATE issues SET owner = $1, updated_at = $2 WHERE id = $3`, actor, updatedAt, current.ID); err != nil {
			return mapSQLError(err, nil)
		}
		body, err := json.Marshal(map[string]any{"owner": actor, "updated_at": updatedAt})
		if err != nil {
			return err
		}
		created, err := s.insertEventTx(ctx, tx, issueEventInput(current, project, "issue.assigned", actor, string(body)))
		if err != nil {
			return err
		}
		result.Event, result.Changed = &created, true
		result.Issue, err = scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, current.ID))
		return err
	})
	return result, err
}

// CloseIssue returns the primary close event from the endpoint-facing event
// envelope.
func (s *Store) CloseIssue(
	ctx context.Context,
	issueID int64,
	reason string,
	actor string,
	message string,
	evidence []db.Evidence,
) (db.Issue, *db.Event, bool, error) {
	issue, events, changed, err := s.CloseIssueWithEvents(ctx, issueID, reason, actor, message, evidence)
	if err != nil || len(events) == 0 {
		return issue, nil, changed, err
	}
	return issue, &events[0], changed, nil
}

// CloseIssueWithEvents returns the ordered event envelope for a close.
func (s *Store) CloseIssueWithEvents(
	ctx context.Context,
	issueID int64,
	reason string,
	actor string,
	message string,
	evidence []db.Evidence,
) (db.Issue, []db.Event, bool, error) {
	return s.closeIssueWithEvents(
		ctx, issueID, reason, actor, message, evidence, s.withSerializableTx,
	)
}

func (s *Store) closeIssueWithEvents(
	ctx context.Context,
	issueID int64,
	reason string,
	actor string,
	message string,
	evidence []db.Evidence,
	runTx func(context.Context, transactionFunc) error,
) (db.Issue, []db.Event, bool, error) {
	if reason == "" {
		return db.Issue{}, nil, false, fmt.Errorf("close reason is required")
	}
	var issue db.Issue
	var events []db.Event
	var changed bool
	err := runTx(ctx, func(tx *sql.Tx) error {
		issue, events, changed = db.Issue{}, nil, false
		current, project, err := lockedIssueTx(ctx, tx, issueID, false)
		if err != nil {
			return err
		}
		if current.Status == "closed" {
			issue = current
			return nil
		}
		var hasOpenChildren bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
          SELECT 1 FROM links l JOIN issues child ON child.id = l.from_issue_id
          JOIN projects cp ON cp.id = child.project_id
          WHERE l.type = 'parent' AND l.to_issue_id = $1 AND child.status = 'open'
            AND child.deleted_at IS NULL AND cp.deleted_at IS NULL)`, current.ID).Scan(&hasOpenChildren); err != nil {
			return mapSQLError(err, nil)
		}
		if hasOpenChildren {
			return db.ErrOpenChildren
		}
		closedAt := mutationTimestamp()
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET status = 'closed', closed_reason = $1,
          closed_at = $2, updated_at = $2 WHERE id = $3`, reason, closedAt, current.ID); err != nil {
			return mapSQLError(err, nil)
		}
		parentUID, parentShortID := new(string), new(string)
		var foundParentUID, foundParentShortID string
		parentErr := tx.QueryRowContext(ctx, `SELECT parent.uid, parent.short_id
          FROM links l JOIN issues parent ON parent.id = l.to_issue_id
          WHERE l.type = 'parent' AND l.from_issue_id = $1`, current.ID).
			Scan(&foundParentUID, &foundParentShortID)
		if parentErr == nil {
			parentUID, parentShortID = &foundParentUID, &foundParentShortID
		} else if !errors.Is(parentErr, sql.ErrNoRows) {
			return mapSQLError(parentErr, nil)
		}
		body, err := json.Marshal(struct {
			Reason        string        `json:"reason"`
			ClosedAt      string        `json:"closed_at"`
			Message       string        `json:"message,omitempty"`
			Evidence      []db.Evidence `json:"evidence,omitempty"`
			ParentUID     *string       `json:"parent_uid,omitempty"`
			ParentShortID *string       `json:"parent_short_id,omitempty"`
		}{
			Reason: reason, ClosedAt: closedAt, Message: message, Evidence: evidence,
			ParentUID: parentUID, ParentShortID: parentShortID,
		})
		if err != nil {
			return err
		}
		created, err := s.insertEventTx(ctx, tx,
			issueEventInput(current, project, "issue.closed", actor, string(body)))
		if err != nil {
			return err
		}
		events, changed = []db.Event{created}, true
		auditEvents, err := s.annotateClaimWorkMutationTx(ctx, tx, claimWorkMutationInput{
			Project: project, Issue: current, EventType: "issue.closed", Actor: actor,
			HolderInstanceUID: s.InstanceUID(), OffendingEventUID: created.UID,
		})
		if err != nil {
			return err
		}
		events = append(events, auditEvents...)
		lastEventID := created.ID
		if len(auditEvents) > 0 {
			lastEventID = auditEvents[len(auditEvents)-1].ID
		}
		if reason == "done" && current.RecurrenceID != nil && current.OccurrenceKey != nil {
			if _, err := s.materializeNextTx(
				ctx, tx, *current.RecurrenceID, *current.OccurrenceKey, actor,
			); err != nil {
				return fmt.Errorf("materialize next recurrence: %w", err)
			}
			generated, err := eventsAfterTx(ctx, tx, lastEventID)
			if err != nil {
				return fmt.Errorf("load recurrence materialization events: %w", err)
			}
			events = append(events, generated...)
		}
		issue, err = scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, current.ID))
		return err
	})
	return issue, events, changed, err
}

// ReopenIssue clears close state; an already-open issue is a no-op.
func (s *Store) ReopenIssue(ctx context.Context, issueID int64, actor string) (db.Issue, *db.Event, bool, error) {
	return s.transitionIssue(ctx, issueID, actor, false)
}

// SoftDeleteIssue hides an issue while retaining all dependent state.
func (s *Store) SoftDeleteIssue(ctx context.Context, issueID int64, actor string) (db.Issue, *db.Event, bool, error) {
	return s.transitionIssue(ctx, issueID, actor, true)
}

// RestoreIssue makes a soft-deleted issue visible again.
func (s *Store) RestoreIssue(ctx context.Context, issueID int64, actor string) (db.Issue, *db.Event, bool, error) {
	var issue db.Issue
	var event *db.Event
	var changed bool
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		issue, event, changed = db.Issue{}, nil, false
		current, project, err := lockedIssueTx(ctx, tx, issueID, true)
		if err != nil {
			return err
		}
		if current.DeletedAt == nil {
			issue = current
			return nil
		}
		restoredAt := mutationTimestamp()
		if _, err := tx.ExecContext(ctx,
			`UPDATE issues SET deleted_at = NULL, updated_at = $1 WHERE id = $2`, restoredAt, current.ID); err != nil {
			return mapSQLError(err, nil)
		}
		body, err := json.Marshal(map[string]any{"restored_at": restoredAt, "updated_at": restoredAt})
		if err != nil {
			return err
		}
		created, err := s.insertEventTx(ctx, tx, issueEventInput(current, project, "issue.restored", actor, string(body)))
		if err != nil {
			return err
		}
		event, changed = &created, true
		issue, err = scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, current.ID))
		return err
	})
	return issue, event, changed, err
}

func (s *Store) transitionIssue(ctx context.Context, issueID int64, actor string, deleteIssue bool) (db.Issue, *db.Event, bool, error) {
	var issue db.Issue
	var event *db.Event
	var changed bool
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		issue, event, changed = db.Issue{}, nil, false
		current, project, err := lockedIssueTx(ctx, tx, issueID, deleteIssue)
		if err != nil {
			return err
		}
		if deleteIssue && current.DeletedAt != nil || !deleteIssue && current.Status == "open" {
			issue = current
			return nil
		}
		at := mutationTimestamp()
		eventType := "issue.reopened"
		payload := map[string]any{"reopened_at": at, "updated_at": at}
		statement := `UPDATE issues SET status = 'open', closed_reason = NULL, closed_at = NULL, updated_at = $1 WHERE id = $2`
		if deleteIssue {
			eventType = "issue.soft_deleted"
			payload = map[string]any{"deleted_at": at}
			statement = `UPDATE issues SET deleted_at = $1, updated_at = $1 WHERE id = $2`
		}
		if _, err := tx.ExecContext(ctx, statement, at, current.ID); err != nil {
			return mapSQLError(err, nil)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		created, err := s.insertEventTx(ctx, tx, issueEventInput(current, project, eventType, actor, string(body)))
		if err != nil {
			return err
		}
		event, changed = &created, true
		issue, err = scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, current.ID))
		return err
	})
	return issue, event, changed, err
}

func lockedIssueTx(ctx context.Context, tx *sql.Tx, issueID int64, includeDeleted bool) (db.Issue, db.Project, error) {
	query := issueSelect + ` WHERE i.id = $1`
	if !includeDeleted {
		query += ` AND i.deleted_at IS NULL`
	}
	query += ` FOR UPDATE OF i`
	issue, err := scanIssue(tx.QueryRowContext(ctx, query, issueID))
	if err != nil {
		return db.Issue{}, db.Project{}, err
	}
	project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id = $1 FOR SHARE`, issue.ProjectID))
	if err != nil {
		return db.Issue{}, db.Project{}, err
	}
	if project.DeletedAt != nil {
		return db.Issue{}, db.Project{}, db.ErrNotFound
	}
	if err := ensureProjectWritableTx(ctx, tx, project.ID); err != nil {
		return db.Issue{}, db.Project{}, err
	}
	return issue, project, nil
}

func issueEventInput(issue db.Issue, project db.Project, eventType, actor, payload string) eventInsert {
	return eventInsert{
		ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
		IssueID: &issue.ID, IssueUID: &issue.UID, Type: eventType, Actor: actor, Payload: payload,
	}
}

func mutationTimestamp() string { return nowStoredTimestamp() }

func equalStringPointers(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalIntPointers(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
