package db

import (
	"context"
	"database/sql"
	"errors"
)

// RunMoveBatchTx executes item savepoints inside a backend-owned transaction.
func RunMoveBatchTx(ctx context.Context, tx *sql.Tx, in MoveIssuesBatchIn, move func(context.Context, *sql.Tx, MoveIssueProjectIn) (MoveIssueProjectOut, error), comment func(context.Context, *sql.Tx, CreateCommentParams) (Comment, Event, error), isTransient func(error) bool) (MoveIssuesBatchOut, bool, error) {
	out := MoveIssuesBatchOut{Results: make([]MoveIssueBatchResult, len(in.Items))}
	moved := false
	succeeded := map[int64]MoveIssueProjectOut{}
	for n, item := range in.Items {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT move_item"); err != nil {
			return out, false, err
		}
		item.DryRun = in.DryRun && !in.Rehearse
		item.Savepoint = true
		itemCtx := ctx
		var err error
		if item.BindContext != nil {
			itemCtx = item.BindContext(ctx)
			err = ApplyTransactionFence(itemCtx, tx)
		}
		var result MoveIssueProjectOut
		prior, duplicate := succeeded[item.IssueID]
		if err == nil && in.Rehearse && duplicate {
			result = prior
			out.Results[n].Duplicate = true
		} else if err == nil {
			result, err = move(itemCtx, tx, item)
		}
		if err == nil && !item.DryRun && !out.Results[n].Duplicate && in.Comment != "" {
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
			if in.Rehearse {
				succeeded[item.IssueID] = result
			}
			moved = moved || !item.DryRun
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT move_item"); err != nil {
			return out, false, err
		}
	}
	return out, moved, nil
}

// FinishMoveBatchAttempt completes rejected items after the transaction ends.
func FinishMoveBatchAttempt(ctx context.Context, out *MoveIssuesBatchOut, transactionErr, rollbackErr error) error {
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
