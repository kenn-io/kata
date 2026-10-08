package pgstore

import (
	"context"
	"database/sql"
	"strconv"

	"go.kenn.io/kata/internal/db"
)

// ReadCommentGraph captures visible comments and endpoint evidence together.
func (s *Store) ReadCommentGraph(ctx context.Context, query db.CommentGraphQuery) (db.CommentGraphData, error) {
	tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return db.CommentGraphData{}, err
	}
	defer func() { _ = tx.Rollback() }()
	query.AllowedIssueIDs, err = readIssueScopeIDs(ctx, tx, query.IssueScope, query.AllowedIssueIDs)
	if err != nil {
		return db.CommentGraphData{}, err
	}
	return readCommentGraphTx(ctx, tx, query)
}

func readCommentGraphTx(ctx context.Context, tx *sql.Tx, query db.CommentGraphQuery) (db.CommentGraphData, error) {
	return db.ReadCommentGraphTx(ctx, tx, query, func(n int) string { return "$" + strconv.Itoa(n) }, parseStoredTime, `(e.payload::jsonb->>'comment_uid')`)
}
