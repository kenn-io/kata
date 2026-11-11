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
	if err != nil {
		return MoveIssuesBatchOut{}, err
	}
	return out, nil
}

// ErrDuplicateMoveTarget reports a repeated issue with a different destination.
var ErrDuplicateMoveTarget = errors.New("issue requested for different targets")

func runMoveBatchTx(ctx context.Context, tx *sql.Tx, in MoveIssuesBatchIn, move func(context.Context, *sql.Tx, MoveIssueProjectIn) (MoveIssueProjectOut, error), comment func(context.Context, *sql.Tx, CreateCommentParams) (Comment, Event, error), isTransient func(error) bool) (MoveIssuesBatchOut, bool, error) {
	out := MoveIssuesBatchOut{Results: make([]MoveIssueBatchResult, len(in.Items))}
	moved := false
	type settledMove struct {
		target, expected int64
		out              MoveIssueProjectOut
	}
	settled := map[int64]settledMove{}
	for n, item := range in.Items {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT move_item"); err != nil {
			return out, false, err
		}
		item.Single = true
		item.DryRun = false
		item.Savepoint = true
		itemCtx := ctx
		var err error
		if item.BindContext != nil {
			itemCtx = item.BindContext(ctx)
			err = ApplyTransactionFence(itemCtx, tx)
		}
		var result MoveIssueProjectOut
		prior, duplicate := settled[item.IssueID]
		unchanged := duplicate || item.FromProjectID == item.ToProjectID
		if err == nil && unchanged {
			if item.AuthorizeProjects != nil {
				err = authorizeIssueMoveProjects(itemCtx, tx, item, []int64{item.FromProjectID, item.ToProjectID})
			}
			var current Issue
			if err == nil {
				err = tx.QueryRowContext(itemCtx, `SELECT id, project_id, revision, short_id, uid FROM issues WHERE id=$1 AND deleted_at IS NULL`, item.IssueID).Scan(&current.ID, &current.ProjectID, &current.Revision, &current.ShortID, &current.UID)
				if errors.Is(err, sql.ErrNoRows) {
					err = ErrNotFound
				}
			}
			if err == nil && duplicate && item.IfMatchRev != prior.expected {
				err = &RevisionConflictError{CurrentRevision: current.Revision}
			}
			if err == nil && duplicate && item.ToProjectID != prior.target {
				err = ErrDuplicateMoveTarget
			}
			want := item.IfMatchRev
			if duplicate {
				want = prior.out.NewRevision
			}
			if err == nil && current.Revision != want {
				err = &RevisionConflictError{CurrentRevision: current.Revision}
			}
			if err == nil && current.ProjectID != item.ToProjectID {
				err = ErrIssueMoveTreeChanged
			}
			if err == nil {
				result = MoveIssueProjectOut{Issue: current, NewShortID: current.ShortID, NewRevision: current.Revision}
			}
		} else if err == nil {
			result, err = move(itemCtx, tx, item)
		}
		if err == nil && !unchanged && in.Comment != "" {
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
			if unchanged {
				out.Results[n].Outcome = "already_in_target"
				if duplicate {
					out.Results[n].Outcome = "duplicate_item"
				}
			} else {
				out.Results[n].Outcome = "moved"
				moved = true
			}
			if !duplicate {
				settled[item.IssueID] = settledMove{target: item.ToProjectID, expected: item.IfMatchRev, out: result}
			}
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
