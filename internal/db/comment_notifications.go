package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
)

// CommentMetadataHook lets the daemon compute notification state using the
// same transaction that creates the comment. The fold never invokes it.
type CommentMetadataHook func(context.Context, *sql.Tx, Issue, Comment) (map[string]jsontext.Value, error)
type commentMetadataContext struct {
	hook      CommentMetadataHook
	committed *[]Event
}
type commentMetadataKey struct{}

// WithCommentMetadataHook retains exact committed events even when a later
// response read fails. A retry clears attempt-local events before writing.
func WithCommentMetadataHook(ctx context.Context, hook CommentMetadataHook, committed *[]Event) context.Context {
	return context.WithValue(ctx, commentMetadataKey{}, commentMetadataContext{hook, committed})
}

// CommentMetadataPolicy returns the daemon's optional comment write policy.
func CommentMetadataPolicy(ctx context.Context) CommentMetadataHook {
	v, _ := ctx.Value(commentMetadataKey{}).(commentMetadataContext)
	return v.hook
}

// RetainCommentEvents replaces the caller's sink with only committed events.
func RetainCommentEvents(ctx context.Context, events ...Event) {
	v, _ := ctx.Value(commentMetadataKey{}).(commentMetadataContext)
	if v.committed != nil {
		*v.committed = append([]Event(nil), events...)
	}
}

// MetadataPatchHook resolves an explicit notification patch after the store
// locks the issue. Broadcast discovery and rate checks therefore serialize.
type MetadataPatchHook func(context.Context, *sql.Tx, Issue) (map[string]jsontext.Value, error)
type metadataPatchKey struct{}

// WithMetadataPatchHook builds a patch from current transaction state.
func WithMetadataPatchHook(ctx context.Context, hook MetadataPatchHook) context.Context {
	return context.WithValue(ctx, metadataPatchKey{}, hook)
}

// ResolveMetadataPatch evaluates a supplied transaction policy or uses the patch.
func ResolveMetadataPatch(ctx context.Context, tx *sql.Tx, issue Issue, patch map[string]jsontext.Value) (map[string]jsontext.Value, error) {
	hook, _ := ctx.Value(metadataPatchKey{}).(MetadataPatchHook)
	if hook == nil {
		return patch, nil
	}
	return hook(ctx, tx, issue)
}
