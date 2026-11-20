package pgstore

import (
	"context"
	"database/sql"
	"errors"

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
	seen := map[int64]bool{}
	for _, id := range append([]int64{fromProject}, toProjects...) {
		if seen[id] {
			continue
		}
		seen[id] = true
		var uid string
		err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, id).Scan(&uid)
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := db.CheckProjectAccessTransaction(ctx, tx, uid); err != nil {
			return err
		}
	}
	return ensureLinkProjectsBoundaryTx(ctx, tx, fromProject, toProjects...)
}

var _ interface {
	CheckLinkBoundary(context.Context, int64, int64) error
} = (*Store)(nil)
