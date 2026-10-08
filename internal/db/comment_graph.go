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
	ProjectID       int64
	AllowedIssueIDs []int64
	IssueScope      *APITokenScope
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

// CommentGraphTarget retains lookup/lineage evidence for unavailable endpoints.
// A moved record must also pass host authorization before API projection.
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

const commentGraphSelect = `SELECT c.id,c.uid,c.issue_id,c.author,c.body,c.created_at,c.teammate,c.reply_to_uid,c.reply_kind,c.edited_at,i.uid,i.short_id,i.project_id,p.uid,p.name FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id`

// ReadCommentGraphTx reads graph inputs using the backend's placeholder and
// timestamp conventions. The caller resolves IssueScope in this transaction.
func ReadCommentGraphTx(ctx context.Context, tx *sql.Tx, query CommentGraphQuery, bind func(int) string, parseTime func(string) (time.Time, error), commentUIDExpr string) (CommentGraphData, error) {
	data := CommentGraphData{Comments: []CommentGraphRecord{}, Targets: map[string]CommentGraphTarget{}, CommentUIDsByProject: map[int64][]string{}}
	if query.AllowedIssueIDs != nil && len(query.AllowedIssueIDs) == 0 {
		return data, nil
	}
	args := []any{query.ProjectID}
	where := " WHERE i.project_id=" + bind(1) + " AND i.deleted_at IS NULL AND p.deleted_at IS NULL"
	if query.AllowedIssueIDs != nil {
		marks := make([]string, len(query.AllowedIssueIDs))
		for n, id := range query.AllowedIssueIDs {
			args = append(args, id)
			marks[n] = bind(len(args))
		}
		where += " AND i.id IN (" + strings.Join(marks, ",") + ")"
	}
	rows, err := tx.QueryContext(ctx, commentGraphSelect+where, args...)
	if err != nil {
		return data, err
	}
	for rows.Next() {
		r, err := scanCommentGraphRecord(rows, parseTime)
		if err != nil {
			_ = rows.Close()
			return data, err
		}
		data.Comments = append(data.Comments, r)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return data, err
	}
	visible := map[string]bool{}
	for _, r := range data.Comments {
		visible[r.Comment.UID] = true
	}
	for n, r := range data.Comments {
		uid := r.Comment.ReplyToUID
		if uid == "" || visible[uid] {
			continue
		}
		if query.AllowedIssueIDs != nil {
			data.Comments[n].Comment.ReplyToUID = ""
			data.Comments[n].Comment.ReplyKind = ""
			continue
		}
		if _, ok := data.Targets[uid]; ok {
			continue
		}
		target, err := scanCommentGraphRecord(tx.QueryRowContext(ctx, commentGraphSelect+" WHERE c.uid="+bind(1)+" AND i.deleted_at IS NULL AND p.deleted_at IS NULL", uid), parseTime)
		if err == nil {
			if _, ok := data.CommentUIDsByProject[target.ProjectID]; !ok {
				// bind emits backend placeholders; the project ID remains a bound value.
				uidRows, err := tx.QueryContext(ctx, `SELECT c.uid FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE i.project_id=`+bind(1)+` AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, target.ProjectID) // #nosec G202
				if err != nil {
					return data, err
				}
				ids := []string{}
				for uidRows.Next() {
					var id string
					if err := uidRows.Scan(&id); err != nil {
						_ = uidRows.Close()
						return data, err
					}
					ids = append(ids, id)
				}
				err = uidRows.Err()
				_ = uidRows.Close()
				if err != nil {
					return data, err
				}
				data.CommentUIDsByProject[target.ProjectID] = ids
			}
			data.Targets[uid] = CommentGraphTarget{Status: "moved", Record: &target}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return data, err
		}
		var status string
		err = tx.QueryRowContext(ctx, `SELECT CASE WHEN p.deleted_at IS NOT NULL THEN 'hidden' WHEN i.deleted_at IS NOT NULL THEN 'removed' ELSE 'pending' END FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE c.uid=`+bind(1), uid).Scan(&status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return data, err
		}
		if status == "" {
			// A retains the source creation event's related UID through purge.
			// Without a tombstone the target may simply not have replicated yet.
			var removed bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events e JOIN purge_log p ON p.issue_uid=e.related_issue_uid WHERE e.type='issue.commented' AND `+commentUIDExpr+"="+bind(1)+")", r.Comment.UID).Scan(&removed)
			if err != nil {
				return data, err
			}
			status = "pending"
			if removed {
				status = "removed"
			}
		}
		data.Targets[uid] = CommentGraphTarget{Status: status}
	}
	return data, nil
}

type commentGraphScanner interface{ Scan(...any) error }

func scanCommentGraphRecord(row commentGraphScanner, parseTime func(string) (time.Time, error)) (CommentGraphRecord, error) {
	var r CommentGraphRecord
	var created string
	var tm, to, kind, edited sql.NullString
	c := &r.Comment
	err := row.Scan(&c.ID, &c.UID, &c.IssueID, &c.Author, &c.Body, &created, &tm, &to, &kind, &edited, &r.IssueUID, &r.IssueShortID, &r.ProjectID, &r.ProjectUID, &r.ProjectName)
	if err != nil {
		return r, err
	}
	c.Teammate, c.ReplyToUID, c.ReplyKind = tm.String, to.String, kind.String
	c.CreatedAt, err = parseTime(created)
	if err != nil {
		return r, fmt.Errorf("parse graph comment time: %w", err)
	}
	if edited.Valid {
		value, err := parseTime(edited.String)
		if err != nil {
			return r, err
		}
		c.EditedAt = &value
	}
	return r, nil
}
