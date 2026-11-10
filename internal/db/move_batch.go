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
	for n, item := range in.Items {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT move_item"); err != nil {
			return MoveIssuesBatchOut{}, false, err
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
		var result MoveIssueProjectOut
		if err == nil {
			if item.FromProjectID == item.ToProjectID {
				err = errors.New("source and target projects are the same")
			} else {
				result, err = move(ctx, tx, item)
			}
		}
		var commentEvent *Event
		if err == nil && !in.DryRun && in.Comment != "" {
			var events []Event
			commentCtx := WithCommentMetadataHook(ctx, CommentMetadataPolicy(ctx), &events)
			for _, issue := range result.Issues {
				var event Event
				_, event, err = comment(commentCtx, tx, CreateCommentParams{IssueID: issue.ID, Author: item.Actor, Body: in.Comment, Teammate: in.Teammate})
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
			if isTransient(err) || ctx.Err() != nil {
				return MoveIssuesBatchOut{}, false, err
			}
			if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT move_item"); rollbackErr != nil {
				return MoveIssuesBatchOut{}, false, errors.Join(err, rollbackErr)
			}
			out.Results[n].Err = err
		} else {
			out.Results[n] = MoveIssueBatchResult{Out: result, CommentEvent: commentEvent, ShortIDExtended: len(result.NewShortID) > len(oldShort)}
			moved = moved || !in.DryRun
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT move_item"); err != nil {
			return MoveIssuesBatchOut{}, false, err
		}
	}
	return out, moved, nil
}
