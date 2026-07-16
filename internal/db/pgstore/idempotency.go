package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/db"
)

// LookupIdempotency finds the newest recent issue.created event carrying key.
func (s *Store) LookupIdempotency(
	ctx context.Context,
	projectID int64,
	key string,
	since time.Time,
) (*db.IdempotencyMatch, error) {
	query := eventSelect + ` WHERE e.type = 'issue.created'
      AND e.project_id = $1
      AND e.payload::jsonb ->> 'idempotency_key' = $2
      AND e.created_at >= $3
      ORDER BY e.id DESC LIMIT 1`
	event, err := scanEvent(s.QueryRowContext(ctx, query, projectID, key, formatStoredTime(since)))
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if event.IssueID == nil {
		return nil, fmt.Errorf("idempotency match has no issue_id")
	}
	issue, err := s.IssueByID(ctx, *event.IssueID)
	if err != nil {
		return nil, fmt.Errorf("read idempotency issue: %w", err)
	}
	var fingerprint sql.NullString
	if err := s.QueryRowContext(ctx,
		`SELECT payload::jsonb ->> 'idempotency_fingerprint' FROM events WHERE id = $1`, event.ID,
	).Scan(&fingerprint); err != nil {
		return nil, mapSQLError(err, nil)
	}
	return &db.IdempotencyMatch{
		IssueID: issue.ID, IssueShortID: issue.ShortID,
		Fingerprint: fingerprint.String, Event: event,
	}, nil
}
