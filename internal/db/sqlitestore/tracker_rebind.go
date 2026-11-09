package sqlitestore

import (
	"context"
	"go.kenn.io/kata/internal/db"
)

func (s *Store) RebindTrackerConnection(ctx context.Context, p db.TrackerRebindParams) (db.IssueSyncBinding, error) {
	if err := validateIssueSyncBindingParams(p.Binding); err != nil {
		return db.IssueSyncBinding{}, err
	}
	return retryWrite1(ctx, s, func() (db.IssueSyncBinding, error) {
		tx, err := s.BeginTx(ctx, nil)
		if err != nil {
			return db.IssueSyncBinding{}, err
		}
		defer func() { _ = tx.Rollback() }()
		if err := requireActiveProject(ctx, tx, p.Binding.ProjectID); err != nil {
			return db.IssueSyncBinding{}, err
		}
		if err := rejectFederationSpokeIssueSyncProject(ctx, tx, p.Binding.ProjectID); err != nil {
			return db.IssueSyncBinding{}, err
		}
		if err := rejectExternalRootIssueSyncSourceTx(ctx, tx, p.Binding.ProjectID, p.Binding.SourceKey); err != nil {
			return db.IssueSyncBinding{}, err
		}
		current, err := issueSyncBindingByProject(ctx, tx, p.Binding.ProjectID)
		if err != nil {
			return db.IssueSyncBinding{}, err
		}
		if err := s.trackerFenceSQL().RebindInTransaction(ctx, tx, current, p); err != nil {
			return db.IssueSyncBinding{}, err
		}
		result, err := issueSyncBindingByProject(ctx, tx, p.Binding.ProjectID)
		if err != nil {
			return db.IssueSyncBinding{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.IssueSyncBinding{}, err
		}
		return result, nil
	})
}
