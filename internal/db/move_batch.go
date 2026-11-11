package db

import (
	"context"
	"database/sql"
	"errors"
)

// RunMoveBatch coordinates the backend's retrying transaction and item savepoints.
func RunMoveBatch(ctx context.Context, in MoveIssuesBatchIn, withTx func(context.Context, func(*sql.Tx) error, func(error, error) error) error, move func(context.Context, *sql.Tx, MoveIssueProjectIn) (MoveIssueProjectOut, error), comment func(context.Context, *sql.Tx, CreateCommentParams) (Comment, Event, error), bump func(context.Context, Transaction) error, isTransient func(error) bool) (MoveIssuesBatchOut, error) {
	var out MoveIssuesBatchOut
	pending := false
	run := func(tx *sql.Tx) error {
		var moved bool
		var err error
		out, moved, err = runMoveBatchTx(ctx, tx, in, move, comment, isTransient)
		pending = true
		if err != nil {
			return err
		}
		if moved {
			if err := bump(ctx, tx); err != nil {
				return err
			}
		}
		if in.DryRun {
			return errBatchMovePreview
		}
		return nil
	}
	finish := func(err, rollbackErr error) error {
		if pending {
			pending = false
			return finishMoveBatchAttempt(ctx, &out, err, rollbackErr)
		}
		return err
	}
	err := withTx(ctx, run, finish)
	if errors.Is(err, errBatchMovePreview) && !errors.Is(err, ErrTransactionFinalizationFailed) {
		err = nil
	}
	if err != nil {
		return MoveIssuesBatchOut{}, err
	}
	return out, nil
}

var errBatchMovePreview = errors.New("batch move preview complete")

func runMoveBatchTx(ctx context.Context, tx *sql.Tx, in MoveIssuesBatchIn, move func(context.Context, *sql.Tx, MoveIssueProjectIn) (MoveIssueProjectOut, error), comment func(context.Context, *sql.Tx, CreateCommentParams) (Comment, Event, error), isTransient func(error) bool) (MoveIssuesBatchOut, bool, error) {
	out := MoveIssuesBatchOut{Results: make([]MoveIssueBatchResult, len(in.Items))}
	moved := false
	for n, item := range in.Items {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT move_item"); err != nil {
			return out, false, err
		}
		item.DryRun = in.DryRun
		item.Savepoint = true
		itemCtx := ctx
		var err error
		if item.BindContext != nil {
			itemCtx = item.BindContext(ctx)
			err = ApplyTransactionFence(itemCtx, tx)
		}
		var result MoveIssueProjectOut
		if err == nil {
			result, err = move(itemCtx, tx, item)
		}
		if err == nil && !item.DryRun && in.Comment != "" {
			var events []Event
			commentCtx := WithCommentMetadataHook(itemCtx, CommentMetadataPolicy(itemCtx), &events)
			for _, issue := range result.Issues {
				_, _, err = comment(commentCtx, tx, CreateCommentParams{IssueID: issue.ID, Author: item.Actor, Body: in.Comment, Teammate: in.Teammate})
				if err != nil {
					break
				}
				result.Events = append(result.Events, events...)
			}
		}
		if err != nil {
			if isTransient(err) || ctx.Err() != nil {
				if _, ok := errors.AsType[*afterRollbackError](err); ok {
					out.Results[n].Err = err
				}
				return out, false, err
			}
			out.Results[n].Err = err
			if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT move_item"); rollbackErr != nil {
				return out, false, errors.Join(err, rollbackErr)
			}
		} else {
			out.Results[n].Out = result
			moved = moved || !item.DryRun
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT move_item"); err != nil {
			return out, false, err
		}
	}
	return out, moved, nil
}

// finishMoveBatchAttempt completes rejected items after the transaction ends.
func finishMoveBatchAttempt(ctx context.Context, out *MoveIssuesBatchOut, transactionErr, rollbackErr error) error {
	for n := range out.Results {
		if out.Results[n].Err == nil {
			continue
		}
		out.Results[n].Err = FinishTransactionRollback(ctx, out.Results[n].Err, rollbackErr)
		if transactionErr != nil && errors.Is(out.Results[n].Err, ErrTransactionFinalizationFailed) {
			transactionErr = errors.Join(transactionErr, out.Results[n].Err)
		}
	}
	return transactionErr
}
