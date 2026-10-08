package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"maps"
	"slices"
)

// CommentMetadataUpdate applies one issue metadata patch as part of the
// transaction that creates a comment.
type CommentMetadataUpdate struct {
	IssueID int64
	Patch   map[string]jsontext.Value
}

// CommentMetadataHook computes transaction-local metadata updates for a
// comment. It is a write policy and is never used while folding events.
type CommentMetadataHook func(context.Context, *sql.Tx, Issue, Comment) ([]CommentMetadataUpdate, error)

type commentMetadataContext struct {
	hook      CommentMetadataHook
	committed *[]Event
}

type commentMetadataKey struct{}

// WithCommentMetadataHook retains the exact committed event batch so the
// caller can publish metadata events together with the comment event.
func WithCommentMetadataHook(ctx context.Context, hook CommentMetadataHook, committed *[]Event) context.Context {
	return context.WithValue(ctx, commentMetadataKey{}, commentMetadataContext{hook: hook, committed: committed})
}

// CommentMetadataPolicy returns the optional transaction-local comment policy.
func CommentMetadataPolicy(ctx context.Context) CommentMetadataHook {
	value, _ := ctx.Value(commentMetadataKey{}).(commentMetadataContext)
	return value.hook
}

// RetainCommentEvents replaces the caller's event sink with the committed
// events from one comment creation attempt.
func RetainCommentEvents(ctx context.Context, events ...Event) {
	value, _ := ctx.Value(commentMetadataKey{}).(commentMetadataContext)
	if value.committed != nil {
		*value.committed = append([]Event(nil), events...)
	}
}

// CoalesceCommentMetadataUpdates combines patches by issue and returns them in
// issue-ID order so one comment transaction emits at most one metadata event
// per affected issue.
func CoalesceCommentMetadataUpdates(updates []CommentMetadataUpdate) []CommentMetadataUpdate {
	patchesByIssue := make(map[int64]map[string]jsontext.Value, len(updates))
	for _, update := range updates {
		patch := patchesByIssue[update.IssueID]
		if patch == nil {
			patch = make(map[string]jsontext.Value, len(update.Patch))
			patchesByIssue[update.IssueID] = patch
		}
		maps.Copy(patch, update.Patch)
	}
	issueIDs := make([]int64, 0, len(patchesByIssue))
	for issueID := range patchesByIssue {
		issueIDs = append(issueIDs, issueID)
	}
	slices.Sort(issueIDs)
	coalesced := make([]CommentMetadataUpdate, 0, len(issueIDs))
	for _, issueID := range issueIDs {
		coalesced = append(coalesced, CommentMetadataUpdate{IssueID: issueID, Patch: patchesByIssue[issueID]})
	}
	return coalesced
}
