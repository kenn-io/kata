package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
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

// CheckLinkProjectsBoundary checks project access and link admission in one transaction.
func (s *Store) CheckLinkProjectsBoundary(ctx context.Context, fromProject int64, toProjects []int64) error {
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		return checkLinkProjectsBoundaryTx(ctx, tx, fromProject, toProjects)
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
