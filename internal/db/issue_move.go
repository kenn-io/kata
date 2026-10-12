package db

import (
	"context"
	"database/sql"
	"fmt"
)

// IssueMoveTreeInputs captures descendant revisions inside the move transaction.
// Both backends serialize the entire discovery/validation/write operation. Parent
// links point from child to parent and may cross project boundaries.
func IssueMoveTreeInputs(ctx context.Context, tx *sql.Tx, in MoveIssueProjectIn) ([]MoveIssueProjectIn, error) {
	if in.Single {
		if in.AuthorizeProjects != nil {
			if err := in.AuthorizeProjects([]int64{in.FromProjectID}); err != nil {
				return nil, err
			}
		}
		return []MoveIssueProjectIn{in}, nil
	}
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE tree(id) AS (
 SELECT id FROM issues WHERE id=$1
 UNION
 SELECT l.from_issue_id FROM links l JOIN tree t ON l.to_issue_id=t.id WHERE l.type='parent'
 ) SELECT i.id, i.project_id, i.revision FROM issues i JOIN tree t ON t.id=i.id
 WHERE i.deleted_at IS NULL ORDER BY CASE WHEN i.id=$1 THEN 0 ELSE 1 END, i.id`, in.IssueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var inputs []MoveIssueProjectIn
	for rows.Next() {
		item := in
		if err := rows.Scan(&item.IssueID, &item.FromProjectID, &item.IfMatchRev); err != nil {
			return nil, err
		}
		if item.IssueID == in.IssueID {
			item = in
		} else if item.FromProjectID == in.ToProjectID {
			continue
		}
		inputs = append(inputs, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(inputs) == 0 || inputs[0].IssueID != in.IssueID {
		return nil, fmt.Errorf("issue %d not in project %d: %w", in.IssueID, in.FromProjectID, ErrNotFound)
	}
	if in.AuthorizeProjects != nil {
		ids := make([]int64, 0, len(inputs))
		for _, item := range inputs {
			ids = append(ids, item.FromProjectID)
		}
		if err := in.AuthorizeProjects(ids); err != nil {
			return nil, err
		}
	}
	return inputs, nil
}

// MovedIssueUID resolves a historical short ID from the existing move journal.
// It follows the stable issue UID even after multiple moves.
func MovedIssueUID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, projectID int64, shortID string, postgres bool) (string, error) {
	fromProject := "json_extract(e.payload, '$.from_project_uid')"
	fromShort := "json_extract(e.payload, '$.from_short_id')"
	if postgres {
		fromProject = "e.payload::jsonb->>'from_project_uid'"
		fromShort = "e.payload::jsonb->>'from_short_id'"
	}
	var uid string
	err := q.QueryRowContext(ctx, `SELECT e.issue_uid FROM events e
 JOIN projects p ON p.uid=`+fromProject+`
 WHERE e.type='issue.moved' AND p.id=$1 AND (`+fromShort+`=$2 OR e.issue_uid=$2)
 ORDER BY e.id DESC LIMIT 1`, projectID, shortID).Scan(&uid)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return uid, err
}
