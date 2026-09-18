package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/db"
)

const defaultAssignmentExpiryLimit = 100

// ExpireAssignments clears a bounded set of due assignments in one project.
// A federated spoke never originates expiry events, even when push is enabled.
func (s *Store) ExpireAssignments(ctx context.Context, p db.ExpireAssignmentsParams) ([]db.Event, error) {
	if p.Limit <= 0 {
		p.Limit = defaultAssignmentExpiryLimit
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	nowText := formatStoredTime(now)
	var events []db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		events = nil
		project, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id = $1 AND deleted_at IS NULL FOR SHARE`, p.ProjectID))
		if err != nil {
			return err
		}
		if err := ensureFederatedSpokeUnsupportedTx(ctx, tx, project.ID); err != nil {
			return err
		}
		type dueAssignment struct {
			issueID   int64
			issueUID  string
			owner     string
			expiresOn storedTime
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id, uid, owner, assignment_expires_on
			  FROM issues
			 WHERE project_id = $1
			   AND deleted_at IS NULL
			   AND owner IS NOT NULL
			   AND assignment_expires_on IS NOT NULL
			   AND assignment_expires_on <= $2
			 ORDER BY assignment_expires_on ASC, id ASC
			 LIMIT $3
			 FOR UPDATE SKIP LOCKED`, project.ID, nowText, p.Limit)
		if err != nil {
			return mapSQLError(err, nil)
		}
		var due []dueAssignment
		for rows.Next() {
			var item dueAssignment
			if err := rows.Scan(&item.issueID, &item.issueUID, &item.owner, &item.expiresOn); err != nil {
				_ = rows.Close()
				return mapSQLError(err, nil)
			}
			due = append(due, item)
		}
		if err := rows.Close(); err != nil {
			return mapSQLError(err, nil)
		}
		if err := rows.Err(); err != nil {
			return mapSQLError(err, nil)
		}
		events = make([]db.Event, 0, len(due))
		for _, item := range due {
			expiryText := formatStoredTime(time.Time(item.expiresOn))
			result, err := tx.ExecContext(ctx, `UPDATE issues
				SET owner = NULL, assignment_expires_on = NULL, updated_at = $1
				WHERE id = $2 AND owner = $3 AND assignment_expires_on = $4 AND deleted_at IS NULL`,
				nowText, item.issueID, item.owner, expiryText)
			if err != nil {
				return mapSQLError(err, nil)
			}
			matched, err := result.RowsAffected()
			if err != nil {
				return mapSQLError(err, nil)
			}
			if matched == 0 {
				continue
			}
			payload, err := json.Marshal(map[string]any{
				"previous_owner": item.owner, "owner": nil,
				"assignment_expires_on": expiryText, "updated_at": nowText,
			})
			if err != nil {
				return fmt.Errorf("marshal assignment expiry: %w", err)
			}
			event, err := s.insertEventTx(ctx, tx, eventInsert{
				ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
				IssueID: &item.issueID, IssueUID: &item.issueUID,
				Type: "issue.assignment_expired", Actor: "system", Payload: string(payload),
			})
			if err != nil {
				return err
			}
			events = append(events, event)
		}
		return nil
	})
	return events, err
}
