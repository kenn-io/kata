package sqlitestore

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/db"
)

const defaultAssignmentExpiryLimit = 100

// ExpireAssignments clears a bounded set of due assignments in one project.
// A federated spoke never originates expiry events, even when push is enabled.
func (d *Store) ExpireAssignments(ctx context.Context, p db.ExpireAssignmentsParams) ([]db.Event, error) {
	return retryWrite1(ctx, d, func() ([]db.Event, error) {
		return d.expireAssignments(ctx, p)
	})
}

func (d *Store) expireAssignments(ctx context.Context, p db.ExpireAssignmentsParams) ([]db.Event, error) {
	if p.Limit <= 0 {
		p.Limit = defaultAssignmentExpiryLimit
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	nowText := now.Format(sqliteTimeFormat)

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	project, err := scanProject(tx.QueryRowContext(ctx,
		projectSelect+` WHERE id = ? AND deleted_at IS NULL`, p.ProjectID))
	if err != nil {
		return nil, err
	}
	if err := ensureFederatedSpokeUnsupportedTx(ctx, tx, project.ID); err != nil {
		return nil, err
	}
	type dueAssignment struct {
		issueID   int64
		issueUID  string
		owner     string
		expiresOn time.Time
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, uid, owner, assignment_expires_on
		  FROM issues
		 WHERE project_id = ?
		   AND deleted_at IS NULL
		   AND owner IS NOT NULL
		   AND assignment_expires_on IS NOT NULL
		   AND assignment_expires_on <= ?
		 ORDER BY assignment_expires_on ASC, id ASC
		 LIMIT ?`, project.ID, nowText, p.Limit)
	if err != nil {
		return nil, fmt.Errorf("list due assignments: %w", err)
	}
	var due []dueAssignment
	for rows.Next() {
		var item dueAssignment
		if err := rows.Scan(&item.issueID, &item.issueUID, &item.owner, &item.expiresOn); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan due assignment: %w", err)
		}
		due = append(due, item)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close due assignments: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due assignments: %w", err)
	}

	events := make([]db.Event, 0, len(due))
	for _, item := range due {
		expiryText := item.expiresOn.UTC().Format(sqliteTimeFormat)
		result, err := tx.ExecContext(ctx, `UPDATE issues
			SET owner = NULL, assignment_expires_on = NULL, revision = revision + 1, updated_at = ?
			WHERE id = ? AND owner = ? AND assignment_expires_on = ? AND deleted_at IS NULL`,
			nowText, item.issueID, item.owner, expiryText)
		if err != nil {
			return nil, fmt.Errorf("expire assignment: %w", err)
		}
		matched, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("expire assignment rows affected: %w", err)
		}
		if matched == 0 {
			continue
		}
		payload, err := json.Marshal(map[string]any{
			"previous_owner": item.owner, "owner": nil,
			"assignment_expires_on": expiryText, "updated_at": nowText,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal assignment expiry: %w", err)
		}
		event, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			IssueID: &item.issueID, IssueUID: &item.issueUID,
			Type: "issue.assignment_expired", Actor: "system", Payload: string(payload),
		})
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit assignment expiry: %w", err)
	}
	return events, nil
}
