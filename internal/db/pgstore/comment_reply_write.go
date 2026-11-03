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
		targetArgs := []any{}
		targetUIDPredicate := db.CommentUIDLookupPredicate("c.uid", p.ReplyToUID, &targetArgs, dollarPlaceholder, db.CommentDisplayPostgres)
		projectPlaceholder := dollarPlaceholder(len(targetArgs) + 1)
		targetArgs = append(targetArgs, source.ProjectID)
		err := tx.QueryRowContext(ctx, `SELECT i.id,i.uid,i.project_id,i.status FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE `+targetUIDPredicate+` AND i.project_id=`+projectPlaceholder+` AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, targetArgs...).Scan(&issue.ID, &issue.UID, &issue.ProjectID, &issue.Status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			target = &issue
		}
		// The predicate is read inside SERIALIZABLE, so competing inserts on
		// different source issues force a retry before either can duplicate it.
		if !p.Force {
			duplicateArgs := []any{source.ProjectID}
			replyTargetPredicate := db.CommentUIDLookupPredicate("reply_to_uid", p.ReplyToUID, &duplicateArgs, dollarPlaceholder, db.CommentDisplayPostgres)
			replyKindPlaceholder := dollarPlaceholder(len(duplicateArgs) + 1)
			duplicateArgs = append(duplicateArgs, p.ReplyKind, p.Author, p.Teammate)
			authorPlaceholder := dollarPlaceholder(len(duplicateArgs) - 1)
			teammatePlaceholder := dollarPlaceholder(len(duplicateArgs))
			comment, err := scanComment(tx.QueryRowContext(ctx, `SELECT `+commentColumns+` FROM comments WHERE issue_id IN (SELECT id FROM issues WHERE project_id=$1) AND `+replyTargetPredicate+` AND reply_kind=`+replyKindPlaceholder+` AND author=`+authorPlaceholder+` AND COALESCE(teammate,'')=`+teammatePlaceholder+` ORDER BY created_at,uid LIMIT 1`, duplicateArgs...))
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
