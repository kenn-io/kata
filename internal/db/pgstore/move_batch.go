package pgstore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// MoveIssuesBatch isolates product refusals with savepoints. Retryable database
// errors abort the whole transaction so no results from an attempt escape.
func (s *Store) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	var out db.MoveIssuesBatchOut
	run := func(tx *sql.Tx) error {
		var moved bool
		var err error
		out, moved, err = db.RunMoveBatchTx(ctx, tx, in, s.moveTreeTx, s.createCommentTx, IsTransient)
		if err != nil {
			return err
		}
		if moved {
			if err := bumpProjectAccess(ctx, tx); err != nil {
				return err
			}
		}
		if in.DryRun {
			return errMovePreview
		}
		return nil
	}
	err := s.withProjectAccessTx(ctx, run)
	if errors.Is(err, errMovePreview) {
		err = nil
	}
	if err != nil {
		return db.MoveIssuesBatchOut{}, err
	}
	// Rejected items are rolled back and the connection has now been returned.
	for n := range out.Results {
		if out.Results[n].Err != nil {
			out.Results[n].Err = db.FinishTransactionRollback(ctx, out.Results[n].Err, nil)
		}
	}
	return out, nil
}
