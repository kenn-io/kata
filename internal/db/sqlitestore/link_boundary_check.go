package sqlitestore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
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

// CheckLinkProjectsBoundary checks project access and link admission in one transaction.
func (d *Store) CheckLinkProjectsBoundary(ctx context.Context, fromProject int64, toProjects []int64) error {
	return d.RetryTransient(ctx, func() error {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := checkLinkProjectsBoundaryTx(ctx, tx, fromProject, toProjects); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func checkLinkProjectsBoundaryTx(ctx context.Context, tx *sql.Tx, fromProject int64, toProjects []int64) error {
	if err := db.CheckLinkProjectsAccess(ctx, tx, fromProject, toProjects); err != nil {
		return err
	}
	return ensureLinkProjectsBoundaryTx(ctx, tx, fromProject, toProjects...)
}

var _ interface {
	CheckLinkBoundary(context.Context, int64, int64) error
} = (*Store)(nil)
