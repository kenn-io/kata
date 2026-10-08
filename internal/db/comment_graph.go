package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CommentGraphQuery scopes a coherent comment read to one active project.
type CommentGraphQuery struct {
	ProjectID                   int64
	AllowedIssueIDs             []int64
	IssueScope                  *APITokenScope
	IncludeDeletedSourceIssueID int64
}

// CommentGraphRecord retains the comment's current owning issue and project.
type CommentGraphRecord struct {
	Comment      Comment
	IssueUID     string
	IssueShortID string
	ProjectID    int64
	ProjectUID   string
	ProjectName  string
}

// CommentGraphTarget retains lookup and lineage evidence for unavailable endpoints.
type CommentGraphTarget struct {
	Status string
	Record *CommentGraphRecord
}

// CommentGraphData is captured wholly inside one backend read transaction.
type CommentGraphData struct {
	CommentUIDsByProject map[int64][]string
	Comments             []CommentGraphRecord
	Targets              map[string]CommentGraphTarget
}

// ReadCommentGraphTx reads graph inputs using the backend's placeholder and
// timestamp conventions. The caller resolves issue scope in the same
// transaction before calling this helper.
func ReadCommentGraphTx(
	ctx context.Context,
	tx *sql.Tx,
	query CommentGraphQuery,
	bind func(int) string,
	parseTime func(string) (time.Time, error),
	commentUIDExpr string,
) (CommentGraphData, error) {
	data := CommentGraphData{
		Comments:             []CommentGraphRecord{},
		Targets:              map[string]CommentGraphTarget{},
		CommentUIDsByProject: map[int64][]string{},
	}
	if query.AllowedIssueIDs != nil && len(query.AllowedIssueIDs) == 0 {
		return data, nil
	}

	args := []any{query.ProjectID}
	where := " WHERE i.project_id=" + bind(1) + " AND p.deleted_at IS NULL AND i.deleted_at IS NULL"
	if query.IncludeDeletedSourceIssueID > 0 {
		args = append(args, query.IncludeDeletedSourceIssueID)
		where = " WHERE i.project_id=" + bind(1) + " AND p.deleted_at IS NULL AND (i.deleted_at IS NULL OR i.id=" + bind(2) + ")"
	}
	if query.AllowedIssueIDs != nil {
		marks := make([]string, len(query.AllowedIssueIDs))
		for index, id := range query.AllowedIssueIDs {
			args = append(args, id)
			marks[index] = bind(len(args))
		}
		where += " AND i.id IN (" + strings.Join(marks, ",") + ")"
	}
	rows, err := tx.QueryContext(ctx, commentGraphSelect+where, args...)
	if err != nil {
		return data, err
	}
	for rows.Next() {
		record, err := scanCommentGraphRecord(rows, parseTime)
		if err != nil {
			_ = rows.Close()
			return data, err
		}
		data.Comments = append(data.Comments, record)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return data, err
	}

	visible := make(map[string]bool, len(data.Comments))
	for _, record := range data.Comments {
		visible[record.Comment.UID] = true
	}
	for index, record := range data.Comments {
		targetUID := record.Comment.ReplyToUID
		if targetUID == "" || visible[targetUID] {
			continue
		}
		if query.AllowedIssueIDs != nil {
			data.Comments[index].Comment.ReplyToUID = ""
			data.Comments[index].Comment.ReplyKind = ""
			continue
		}
		if _, alreadyResolved := data.Targets[targetUID]; alreadyResolved {
			continue
		}

		target, err := scanCommentGraphRecord(tx.QueryRowContext(ctx,
			commentGraphSelect+" WHERE c.uid="+bind(1)+" AND i.deleted_at IS NULL AND p.deleted_at IS NULL", targetUID), parseTime)
		if err == nil {
			if _, loaded := data.CommentUIDsByProject[target.ProjectID]; !loaded {
				uidRows, err := tx.QueryContext(ctx, `SELECT c.uid FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE i.project_id=`+bind(1)+` AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, target.ProjectID) // #nosec G202
				if err != nil {
					return data, err
				}
				uids := []string{}
				for uidRows.Next() {
					var uid string
					if err := uidRows.Scan(&uid); err != nil {
						_ = uidRows.Close()
						return data, err
					}
					uids = append(uids, uid)
				}
				err = uidRows.Err()
				_ = uidRows.Close()
				if err != nil {
					return data, err
				}
				data.CommentUIDsByProject[target.ProjectID] = uids
			}
			data.Targets[targetUID] = CommentGraphTarget{Status: "moved", Record: &target}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return data, err
		}

		var status string
		err = tx.QueryRowContext(ctx, `SELECT CASE WHEN p.deleted_at IS NOT NULL THEN 'hidden' WHEN i.deleted_at IS NOT NULL THEN 'removed' ELSE 'pending' END FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE c.uid=`+bind(1), targetUID).Scan(&status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return data, err
		}
		if status == "" {
			var removed bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events e JOIN purge_log p ON p.issue_uid=e.related_issue_uid WHERE e.type='issue.commented' AND `+commentUIDExpr+"="+bind(1)+")", record.Comment.UID).Scan(&removed)
			if err != nil {
				return data, err
			}
			status = "pending"
			if removed {
				status = "removed"
			}
		}
		data.Targets[targetUID] = CommentGraphTarget{Status: status}
	}
	return data, nil
}

type commentGraphScanner interface{ Scan(...any) error }

const commentGraphSelect = `SELECT c.id,c.uid,c.issue_id,c.author,c.body,c.created_at,c.teammate,c.reply_to_uid,c.reply_kind,c.edited_at,i.uid,i.short_id,i.project_id,p.uid,p.name FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id`

func scanCommentGraphRecord(row commentGraphScanner, parseTime func(string) (time.Time, error)) (CommentGraphRecord, error) {
	var record CommentGraphRecord
	var created string
	var teammate, replyTo, replyKind, edited sql.NullString
	comment := &record.Comment
	err := row.Scan(&comment.ID, &comment.UID, &comment.IssueID, &comment.Author, &comment.Body,
		&created, &teammate, &replyTo, &replyKind, &edited, &record.IssueUID,
		&record.IssueShortID, &record.ProjectID, &record.ProjectUID, &record.ProjectName)
	if err != nil {
		return record, err
	}
	comment.Teammate, comment.ReplyToUID, comment.ReplyKind = teammate.String, replyTo.String, replyKind.String
	comment.CreatedAt, err = parseTime(created)
	if err != nil {
		return record, fmt.Errorf("parse graph comment time: %w", err)
	}
	if edited.Valid {
		value, err := parseTime(edited.String)
		if err != nil {
			return record, err
		}
		comment.EditedAt = &value
	}
	return record, nil
}

// CommentGraphContainsID reports whether ids contains target.
func CommentGraphContainsID(ids []int64, target int64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// CommentGraphIntersectIDs intersects scoped issue IDs with an optional
// caller-provided allowlist. A nil allowlist preserves all scoped IDs.
func CommentGraphIntersectIDs(scopeIDs, allowedIDs []int64) []int64 {
	if allowedIDs == nil {
		return scopeIDs
	}
	intersection := make([]int64, 0, len(scopeIDs))
	for _, id := range scopeIDs {
		if CommentGraphContainsID(allowedIDs, id) {
			intersection = append(intersection, id)
		}
	}
	return intersection
}

// CommentGraphAddID appends id once, preserving the existing order.
func CommentGraphAddID(ids []int64, id int64) []int64 {
	if CommentGraphContainsID(ids, id) {
		return ids
	}
	return append(ids, id)
}

// CommentGraphDeletedSourceInScope checks the selected deleted issue's current
// parent ancestry in the same snapshot as comment and target reads.
func CommentGraphDeletedSourceInScope(ctx context.Context, tx *sql.Tx, sourceID, projectID int64, scope APITokenScope, bind func(int) string) (bool, error) {
	query := `WITH RECURSIVE ancestors(id, uid, project_id) AS (
 SELECT i.id, i.uid, i.project_id FROM issues i JOIN projects p ON p.id=i.project_id
 WHERE i.id=` + bind(1) + ` AND i.project_id=` + bind(2) + ` AND i.deleted_at IS NOT NULL AND p.deleted_at IS NULL
 UNION
 SELECT parent.id, parent.uid, parent.project_id FROM ancestors child
 JOIN links edge ON edge.from_issue_id=child.id AND edge.type='parent'
 JOIN issues parent ON parent.id=edge.to_issue_id AND parent.project_id=child.project_id
 JOIN projects p ON p.id=parent.project_id
 WHERE parent.deleted_at IS NULL AND p.deleted_at IS NULL
)
SELECT EXISTS(
 SELECT 1 FROM ancestors a
 JOIN issues root ON root.id=a.id AND root.uid=` + bind(3) + ` AND root.deleted_at IS NULL
 JOIN projects p ON p.id=root.project_id AND p.uid=` + bind(4) + ` AND p.deleted_at IS NULL
 WHERE a.project_id=p.id
)`
	var included bool
	err := tx.QueryRowContext(ctx, query, sourceID, projectID, scope.RootIssueUID, scope.ProjectUID).Scan(&included)
	return included, err
}
