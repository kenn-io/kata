package sqlitestore

import (
	"context"
)

// CheckLinkBoundary checks the current endpoint projects inside one transaction.
func (d *Store) CheckLinkBoundary(ctx context.Context, fromIssueID, toIssueID int64) error {
	return d.RetryTransient(ctx, func() error {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := checkLinkEndpointsProjectAccessTx(ctx, tx, fromIssueID, toIssueID); err != nil {
			return err
		}
		if err := ensureRelayLinkBoundaryTx(ctx, tx, fromIssueID, toIssueID); err != nil {
			return err
		}
		return tx.Commit()
	})
}

var _ interface {
	CheckLinkBoundary(context.Context, int64, int64) error
} = (*Store)(nil)
