package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
)

// makeProjectResolver returns a hook ProjectResolver backed by the live
// DB. Only the human-readable name is needed; id and identity already
// live on the Event itself.
func makeProjectResolver(store db.Storage) func(context.Context, int64) (hooks.ProjectSnapshot, error) {
	return func(ctx context.Context, id int64) (hooks.ProjectSnapshot, error) {
		p, err := store.ProjectByID(ctx, id)
		if err != nil {
			return hooks.ProjectSnapshot{}, fmt.Errorf("project %d: %w", id, err)
		}
		return hooks.ProjectSnapshot{Name: p.Name}, nil
	}
}

// makeIssueResolver returns a hook IssueResolver. Owner is *string in
// the DB row; the snapshot exposes "" for the unassigned case so the
// payload assembler can emit the empty string consistently.
func makeIssueResolver(store db.Storage) func(context.Context, int64) (hooks.IssueSnapshot, error) {
	return func(ctx context.Context, id int64) (hooks.IssueSnapshot, error) {
		issue, err := store.IssueByID(ctx, id)
		if err != nil {
			return hooks.IssueSnapshot{}, fmt.Errorf("issue %d: %w", id, err)
		}
		labels, err := store.LabelsForIssue(ctx, id)
		if err != nil {
			return hooks.IssueSnapshot{}, fmt.Errorf("labels for issue %d: %w", id, err)
		}
		owner := ""
		if issue.Owner != nil {
			owner = *issue.Owner
		}
		return hooks.IssueSnapshot{
			UID:     issue.UID,
			ShortID: issue.ShortID,
			Title:   issue.Title,
			Status:  issue.Status,
			Labels:  labels,
			Owner:   owner,
			Author:  issue.Author,
		}, nil
	}
}

// makeCommentResolver returns a hook CommentResolver. sql.ErrNoRows is
// translated into a typed not-found message so the dispatcher's log
// gets a clearer hint than "no rows in result set".
func makeCommentResolver(store db.Storage) func(context.Context, int64) (hooks.CommentSnapshot, error) {
	return func(ctx context.Context, id int64) (hooks.CommentSnapshot, error) {
		body, err := store.CommentBodyByID(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return hooks.CommentSnapshot{}, fmt.Errorf("comment %d: not found", id)
			}
			return hooks.CommentSnapshot{}, err
		}
		return hooks.CommentSnapshot{ID: id, Body: body}, nil
	}
}

// makeAliasResolver returns a hook AliasResolver. The most-recently-seen
// alias for the event's project is reported. Projects with zero aliases
// produce (_, false, nil) and the alias block is omitted from the hook
// payload.
func makeAliasResolver(store db.Storage) func(context.Context, db.Event) (hooks.AliasSnapshot, bool, error) {
	return func(ctx context.Context, evt db.Event) (hooks.AliasSnapshot, bool, error) {
		alias, has, err := store.LatestAliasForProject(ctx, evt.ProjectID)
		if err != nil {
			return hooks.AliasSnapshot{}, false, err
		}
		if !has {
			return hooks.AliasSnapshot{}, false, nil
		}
		return hooks.AliasSnapshot{
			Identity: alias.Identity,
			Kind:     alias.Kind,
			RootPath: alias.RootPath,
		}, true, nil
	}
}
