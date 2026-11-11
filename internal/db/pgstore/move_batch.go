package pgstore

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

// MoveIssuesBatch isolates product refusals with savepoints. Retryable database
// errors abort the whole transaction so no results from an attempt escape.
func (s *Store) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	return db.RunMoveBatch(ctx, in, s.withProjectAccessTxFinalized, s.moveTreeTx, s.createCommentTx, bumpProjectAccess, IsTransient)
}
