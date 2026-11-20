package pgstore

import (
	"context"
	"database/sql"
)

// CheckLinkBoundary checks the current endpoint projects inside one transaction.
func (s *Store) CheckLinkBoundary(ctx context.Context, fromIssueID, toIssueID int64) error {
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		if err := checkLinkEndpointsProjectAccessTx(ctx, tx, fromIssueID, toIssueID); err != nil {
			return err
		}
		return ensureRelayLinkBoundaryTx(ctx, tx, fromIssueID, toIssueID)
	})
}

var _ interface {
	CheckLinkBoundary(context.Context, int64, int64) error
} = (*Store)(nil)
