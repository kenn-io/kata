package sqlitestore

import (
	"context"
	"database/sql"
	"time"

	"go.kenn.io/kata/internal/db"
)

// ReadCommentGraph captures visible comments and endpoint evidence together.
func (d *Store) ReadCommentGraph(ctx context.Context, query db.CommentGraphQuery) (db.CommentGraphData, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
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
	return db.ReadCommentGraphTx(ctx, tx, query, func(int) string { return "?" }, func(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }, `json_extract(e.payload,'$.comment_uid')`)
}
