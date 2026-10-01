package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

var _ db.IssueStatusLocatorStore = (*Store)(nil)

func saveIssueStatusLocatorTx(ctx context.Context, tx *sql.Tx, guard db.IssueSyncImportGuard, locator db.IssueStatusLocator) (bool, error) {
	if guard.Provider != "github" || locator.ExternalID == "" || locator.Locator == "" || len(locator.LegacyExternalIDs) > 2 {
		return false, fmt.Errorf("%w: invalid verified status locator", db.ErrImportValidation)
	}
	b, err := statusReadBindingTx(ctx, tx, guard)
	if err != nil {
		return false, err
	}
	m, found, err := adoptImportMappingTx(ctx, tx, b.ProjectID, b.SourceKey, "issue", locator.ExternalID, locator.LegacyExternalIDs)
	if err != nil || !found {
		return false, err
	}
	current, err := loadIssueStatusMappingTx(ctx, tx, b, m.ID)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current.State.RemoteLocator == locator.Locator {
		return true, nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE import_mappings SET remote_locator=$1 WHERE id=$2`, locator.Locator, m.ID)
	return err == nil, err
}

// SaveIssueStatusLocator persists a provider-verified remote issue locator.
func (s *Store) SaveIssueStatusLocator(ctx context.Context, guard db.IssueSyncImportGuard, locator db.IssueStatusLocator) (bool, error) {
	var saved bool
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var err error
		saved, err = saveIssueStatusLocatorTx(ctx, tx, guard, locator)
		return err
	})
	return saved, err
}
