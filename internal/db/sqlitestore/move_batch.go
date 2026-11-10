package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
)

// MoveIssuesBatch isolates product refusals with savepoints. Retryable database
// errors abort the whole transaction so no results from an attempt escape.
func (d *Store) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	if err := teammate.Validate(in.Teammate); err != nil {
		return db.MoveIssuesBatchOut{}, err
	}
	var out db.MoveIssuesBatchOut
	run := func(tx *sql.Tx) error {
		out = db.MoveIssuesBatchOut{Results: make([]db.MoveIssueBatchResult, len(in.Items))}
		if err := lockProjectAccess(ctx, tx); err != nil {
			return err
		}
		moved := false
		for n, item := range in.Items {
			if _, err := tx.ExecContext(ctx, "SAVEPOINT move_item"); err != nil {
				return err
			}
			item.DryRun = in.DryRun
			item.Savepoint = true
			if in.Actor != "" {
				item.Actor = in.Actor
			}
			if in.AuthorizeProjects != nil {
				item.AuthorizeProjects = in.AuthorizeProjects
			}
			var oldShort string
			err := tx.QueryRowContext(ctx, "SELECT short_id FROM issues WHERE id=$1", item.IssueID).Scan(&oldShort)
			var result db.MoveIssueProjectOut
			if err == nil {
				if item.FromProjectID == item.ToProjectID {
					err = errors.New("source and target projects are the same")
				} else {
					result, err = d.moveTreeTx(ctx, tx, item)
				}
			}
			var commentEvent *db.Event
			if err == nil && !in.DryRun && in.Comment != "" {
				var events []db.Event
				commentCtx := db.WithCommentMetadataHook(ctx, db.CommentMetadataPolicy(ctx), &events)
				for _, issue := range result.Issues {
					var event db.Event
					_, event, err = d.createCommentTx(commentCtx, tx, db.CreateCommentParams{IssueID: issue.ID, Author: item.Actor, Body: in.Comment, Teammate: in.Teammate})
					if err != nil {
						break
					}
					if commentEvent == nil {
						commentEvent = &event
					}
					result.Events = append(result.Events, events...)
				}
			}
			if err != nil {
				if IsTransient(err) || ctx.Err() != nil {
					return err
				}
				if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT move_item"); rollbackErr != nil {
					return errors.Join(err, rollbackErr)
				}
				out.Results[n].Err = err
			} else {
				out.Results[n] = db.MoveIssueBatchResult{Out: result, CommentEvent: commentEvent, ShortIDExtended: len(result.NewShortID) > len(oldShort)}
				moved = moved || !in.DryRun
			}
			if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT move_item"); err != nil {
				return err
			}
		}
		if moved {
			if err := bumpProjectAccess(ctx, tx); err != nil {
				return err
			}
		}
		if in.DryRun {
			return errBatchMovePreview
		}
		return nil
	}
	err := d.RetryTransient(ctx, func() error {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := run(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
	if errors.Is(err, errBatchMovePreview) {
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

var errBatchMovePreview = errors.New("batch move preview complete")

// CheckIssueMovePair checks the shared federation gate before collecting claims.
// The write transaction independently revalidates the gate under its access lock.
func (d *Store) CheckIssueMovePair(ctx context.Context, from, to int64) error {
	return db.CheckFederatedIssueMove(ctx, d, from, to)
}
