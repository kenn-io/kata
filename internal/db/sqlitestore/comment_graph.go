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

	requestedAllowed := query.AllowedIssueIDs
	if query.IssueScope != nil {
		scopeIDs, err := readIssueScopeIDs(ctx, tx, query.IssueScope, nil)
		if err != nil {
			return db.CommentGraphData{}, err
		}
		query.AllowedIssueIDs = db.CommentGraphIntersectIDs(scopeIDs, requestedAllowed)
		if query.IncludeDeletedSourceIssueID > 0 &&
			(requestedAllowed == nil || db.CommentGraphContainsID(requestedAllowed, query.IncludeDeletedSourceIssueID)) {
			allowed, err := db.CommentGraphDeletedSourceInScope(ctx, tx, query.IncludeDeletedSourceIssueID,
				query.ProjectID, *query.IssueScope, func(_ int) string { return "?" })
			if err != nil {
				return db.CommentGraphData{}, err
			}
			if allowed {
				query.AllowedIssueIDs = db.CommentGraphAddID(query.AllowedIssueIDs, query.IncludeDeletedSourceIssueID)
			}
		}
	} else if requestedAllowed != nil {
		query.AllowedIssueIDs = requestedAllowed
		if query.IncludeDeletedSourceIssueID > 0 && db.CommentGraphContainsID(requestedAllowed, query.IncludeDeletedSourceIssueID) {
			query.AllowedIssueIDs = db.CommentGraphAddID(query.AllowedIssueIDs, query.IncludeDeletedSourceIssueID)
		}
	}

	data, err := db.ReadCommentGraphTx(ctx, tx, query,
		func(int) string { return "?" },
		func(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) },
		`json_extract(e.payload,'$.reply_to_uid')`,
	)
	if err != nil {
		return data, err
	}
	if err := tx.Commit(); err != nil {
		return db.CommentGraphData{}, err
	}
	return data, nil
}
