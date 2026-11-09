package pgstore

import (
	"context"
	"database/sql"
	"go.kenn.io/kata/internal/db"
)

func (s *Store) RebindTrackerConnection(ctx context.Context, p db.TrackerRebindParams) (db.IssueSyncBinding, error) {
	if err := validateIssueSyncBindingParams(p.Binding); err != nil {
		return db.IssueSyncBinding{}, err
	}
	var result db.IssueSyncBinding
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		result = db.IssueSyncBinding{}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, p.Binding.ProjectID); err != nil {
			return mapSQLError(err, nil)
		}
		if _, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, p.Binding.ProjectID)); err != nil {
			return err
		}
		if err := rejectFederationSpokeIssueSyncProject(ctx, tx, p.Binding.ProjectID); err != nil {
			return err
		}
		if err := rejectExternalRootIssueSyncSourceTx(ctx, tx, p.Binding.ProjectID, p.Binding.SourceKey); err != nil {
			return err
		}
		current, err := scanIssueSyncBinding(tx.QueryRowContext(ctx, issueSyncBindingSelect+` WHERE b.project_id=$1 FOR UPDATE`, p.Binding.ProjectID))
		if err != nil {
			return err
		}
		if err := s.trackerFenceSQL().RebindInTransaction(ctx, tx, current, p); err != nil {
			return err
		}
		result, err = scanIssueSyncBinding(tx.QueryRowContext(ctx, issueSyncBindingSelect+` WHERE b.project_id=$1`, p.Binding.ProjectID))
		return err
	})
	return result, err
}
