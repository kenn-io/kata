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
	pending := false
	run := func(tx *sql.Tx) error {
		var moved bool
		var err error
		out, moved, err = db.RunMoveBatchTx(ctx, tx, in, s.moveTreeTx, s.createCommentTx, IsTransient)
		pending = true
		if err != nil {
			return err
		}
		if moved {
			if err := bumpProjectAccess(ctx, tx); err != nil {
				return err
			}
		}
		if in.DryRun || in.Rehearse {
			return errMovePreview
		}
		return nil
	}
	finish := func(err, rollbackErr error) error {
		if in.Rehearse && rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, db.ErrTransactionFinalizationFailed, rollbackErr)
		}
		if pending {
			pending = false
			return db.FinishMoveBatchAttempt(ctx, &out, err, rollbackErr)
		}
		return err
	}
	err := s.withProjectAccessTxFinalized(ctx, run, finish)
	if errors.Is(err, errMovePreview) && !errors.Is(err, db.ErrTransactionFinalizationFailed) {
		err = nil
	}
	if err != nil {
		return db.MoveIssuesBatchOut{}, err
	}
	return out, nil
}
