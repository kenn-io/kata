package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
)

// MetadataPatchHook resolves an issue metadata patch from the current issue
// state while the store's write transaction is open.
type MetadataPatchHook func(context.Context, *sql.Tx, Issue) (map[string]jsontext.Value, error)

type metadataPatchKey struct{}

// WithMetadataPatchHook installs a transaction-local resolver for one
// PatchIssueMetadata or ApplyExternalFieldProjection call.
func WithMetadataPatchHook(ctx context.Context, hook MetadataPatchHook) context.Context {
	return context.WithValue(ctx, metadataPatchKey{}, hook)
}

// ResolveMetadataPatch applies the configured transaction-local resolver, or
// returns the explicit patch when no resolver is configured.
func ResolveMetadataPatch(
	ctx context.Context,
	tx *sql.Tx,
	issue Issue,
	patch map[string]jsontext.Value,
) (map[string]jsontext.Value, error) {
	hook, _ := ctx.Value(metadataPatchKey{}).(MetadataPatchHook)
	if hook == nil {
		return patch, nil
	}
	return hook(ctx, tx, issue)
}

// IssueRecurrenceTimezone resolves an occurrence's inherited zone in its
// metadata write transaction. A missing recurrence leaves the normal fallback.
func IssueRecurrenceTimezone(ctx context.Context, tx *sql.Tx, issue Issue) (string, error) {
	if issue.RecurrenceID == nil {
		return "", nil
	}
	var zone string
	err := tx.QueryRowContext(ctx,
		`SELECT timezone FROM recurrences WHERE id = $1 AND project_id = $2`,
		*issue.RecurrenceID, issue.ProjectID).Scan(&zone)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return zone, err
}
