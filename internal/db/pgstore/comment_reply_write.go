package pgstore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

func validateLocalCommentReplyTx(ctx context.Context, tx *sql.Tx, uid string, p db.CreateCommentParams, source db.Issue) error {
	if !p.ValidateReply {
		return nil
	}
	var target *db.Issue
	var duplicate *db.Comment
	if p.ReplyToUID != "" {
		var issue db.Issue
		err := tx.QueryRowContext(ctx, `SELECT i.id,i.uid,i.project_id,i.status FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE c.uid=$1 AND i.project_id=$2 AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, p.ReplyToUID, source.ProjectID).Scan(&issue.ID, &issue.UID, &issue.ProjectID, &issue.Status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			target = &issue
		}
		// The predicate is read inside SERIALIZABLE, so competing inserts on
		// different source issues force a retry before either can duplicate it.
		if !p.Force {
			comment, err := scanComment(tx.QueryRowContext(ctx, `SELECT `+commentColumns+` FROM comments WHERE issue_id IN (SELECT id FROM issues WHERE project_id=$1) AND reply_to_uid=$2 AND reply_kind=$3 AND author=$4 AND COALESCE(teammate,'')=$5 ORDER BY created_at,uid LIMIT 1`, source.ProjectID, p.ReplyToUID, p.ReplyKind, p.Author, p.Teammate))
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				return err
			}
			if err == nil {
				duplicate = &comment
			}
		}
	}
	return db.ValidateLocalCommentReply(uid, p, source, target, duplicate)
}
