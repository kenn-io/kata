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
			comment, err := scanComment(tx.QueryRowContext(ctx, `SELECT c.id,c.uid,c.issue_id,c.author,c.body,c.created_at,c.teammate,c.reply_to_uid,c.reply_kind,c.edited_at FROM comments c JOIN issues i ON i.id=c.issue_id WHERE i.project_id=$1 AND c.reply_to_uid=$2 AND c.reply_kind=$3 AND c.author=$4 AND COALESCE(c.teammate,'')=$5 ORDER BY c.created_at,c.uid LIMIT 1`, source.ProjectID, p.ReplyToUID, p.ReplyKind, p.Author, p.Teammate))
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
