package sqlitestore

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

// MoveIssuesBatch isolates product refusals with savepoints. Retryable database
// errors abort the whole transaction so no results from an attempt escape.
func (d *Store) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	return db.RunMoveBatch(ctx, in, d.withProjectAccessTxFinalized, d.moveTreeTx, d.createCommentTx, bumpProjectAccess, IsTransient)
}
