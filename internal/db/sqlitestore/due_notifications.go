package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// ListDueNotificationIssueIDs returns live open issues that have a planning
// date or inbox metadata that may need automatic cleanup. Enabled spokes are
// projections of their hub and never originate these writes.
func (d *Store) ListDueNotificationIssueIDs(ctx context.Context) ([]int64, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT i.id
		  FROM issues i
		  JOIN projects p ON p.id = i.project_id
		  LEFT JOIN federation_bindings fb ON fb.project_id = p.id
		 WHERE i.deleted_at IS NULL
		   AND p.deleted_at IS NULL
		   AND i.status = 'open'
		   AND NOT (COALESCE(fb.enabled, 0) = 1 AND fb.role = 'spoke')
		   AND (
			 json_type(i.metadata, '$.scheduled_on') IS NOT NULL
			 OR json_type(i.metadata, '$.deadline_on') IS NOT NULL
			 OR EXISTS (
				 SELECT 1 FROM json_each(i.metadata) entry
				  WHERE entry.key LIKE 'notify.%'
			 )
		   )
		 ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("list due notification issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan due notification issue: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReconcileDueNotification atomically rereads one issue, plans an ordinary
// notify.* metadata patch from its dates and event history, and emits the same
// metadata event as a manual patch.
func (d *Store) ReconcileDueNotification(ctx context.Context, in db.ReconcileDueNotificationIn) (db.ReconcileDueNotificationOut, error) {
	return retryWrite1(ctx, d, func() (db.ReconcileDueNotificationOut, error) {
		return d.reconcileDueNotification(ctx, in)
	})
}

func (d *Store) reconcileDueNotification(ctx context.Context, in db.ReconcileDueNotificationIn) (db.ReconcileDueNotificationOut, error) {
	var out db.ReconcileDueNotificationOut
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, recurrenceTimezone, err := scanScheduledIssue(tx.QueryRowContext(ctx,
		scheduledIssueSelect+` WHERE i.id = ? AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, in.IssueID))
	if err != nil {
		return out, err
	}
	if issue.Status != "open" {
		return commitDueNotificationNoop(tx, out)
	}
	enabledSpoke, err := enabledSpokeProjectTx(ctx, tx, issue.ProjectID)
	if err != nil {
		return out, err
	}
	if enabledSpoke {
		return commitDueNotificationNoop(tx, out)
	}

	history, err := issueMetadataHistoryTx(ctx, tx, issue.ID)
	if err != nil {
		return out, err
	}
	owner := ""
	if issue.Owner != nil {
		owner = *issue.Owner
	}
	patch, err := db.PlanDueNotification(
		json.RawMessage(issue.Metadata), owner, issue.Author, in.Now,
		scheduleDefaultTimezone(recurrenceTimezone, in.DefaultTimezone), history,
	)
	if err != nil {
		return out, err
	}
	if len(patch) == 0 {
		return commitDueNotificationNoop(tx, out)
	}
	for key, raw := range patch {
		if err := metadata.Validate(metadata.IssueRegistry, key, raw); err != nil {
			return out, fmt.Errorf("validate %q: %w", key, err)
		}
	}
	newBlob, err := db.ApplyMetadataPatch(json.RawMessage(issue.Metadata), patch)
	if err != nil {
		return out, fmt.Errorf("apply due notification patch: %w", err)
	}
	diff, err := metadata.Diff(json.RawMessage(issue.Metadata), newBlob)
	if err != nil {
		return out, fmt.Errorf("compute due notification diff: %w", err)
	}
	if len(diff) == 0 {
		return commitDueNotificationNoop(tx, out)
	}

	newRevision := issue.Revision + 1
	timestamp := nowTimestamp()
	if _, err := tx.ExecContext(ctx, `
		UPDATE issues
		   SET metadata = ?, revision = ?, updated_at = ?
		 WHERE id = ?`, string(newBlob), newRevision, timestamp, issue.ID); err != nil {
		return out, fmt.Errorf("update due notification metadata: %w", err)
	}
	payload, err := marshalIssueMetadataUpdatePayload(diff, newRevision, timestamp)
	if err != nil {
		return out, fmt.Errorf("marshal due notification event: %w", err)
	}
	var projectName string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM projects WHERE id = ?`, issue.ProjectID).Scan(&projectName); err != nil {
		return out, fmt.Errorf("read due notification project: %w", err)
	}
	event, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID: issue.ProjectID, ProjectName: projectName, IssueID: &issue.ID,
		Type: "issue.metadata_updated", Actor: "system", Payload: string(payload),
	})
	if err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	out.Event = &event
	out.Changed = true
	return out, nil
}

func commitDueNotificationNoop(tx *sql.Tx, out db.ReconcileDueNotificationOut) (db.ReconcileDueNotificationOut, error) {
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}

func enabledSpokeProjectTx(ctx context.Context, tx *sql.Tx, projectID int64) (bool, error) {
	var role string
	var enabled int
	err := tx.QueryRowContext(ctx,
		`SELECT role, enabled FROM federation_bindings WHERE project_id = ?`, projectID,
	).Scan(&role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check due notification federation role: %w", err)
	}
	return enabled == 1 && role == string(db.FederationRoleSpoke), nil
}

func issueMetadataHistoryTx(ctx context.Context, tx *sql.Tx, issueID int64) ([]db.Event, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT type, payload
		  FROM events
		 WHERE issue_id = ?
		   AND type IN ('issue.created', 'issue.snapshot', 'issue.metadata_updated')
		 ORDER BY id`, issueID)
	if err != nil {
		return nil, fmt.Errorf("read due notification history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var history []db.Event
	for rows.Next() {
		var event db.Event
		if err := rows.Scan(&event.Type, &event.Payload); err != nil {
			return nil, fmt.Errorf("scan due notification history: %w", err)
		}
		history = append(history, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due notification history: %w", err)
	}
	return history, nil
}
