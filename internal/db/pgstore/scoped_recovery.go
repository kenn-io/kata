package pgstore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// validateScopedRestoreTx runs before replacement commits. Retained proof
// validation is deliberately independent of live serving/grant eligibility.
// Explicitly fenced owners remain non-serving until a later verified rebuild.
func (s *Store) validateScopedRestoreTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT owner_view_uid,owner_generation FROM federation_scoped_projects WHERE owner_view_uid IS NOT NULL AND serving_fenced=0 ORDER BY owner_view_uid,owner_generation`)
	if err != nil {
		return err
	}
	keys := []db.ScopedViewKey{}
	for rows.Next() {
		var key db.ScopedViewKey
		if err = rows.Scan(&key.ViewUID, &key.Generation); err != nil {
			break
		}
		keys = append(keys, key)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil || len(keys) == 0 {
		return err
	}
	var instance string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance_uid'`).Scan(&instance); err != nil {
		return err
	}
	for _, key := range keys {
		materialized, err := s.loadScopedMaterializationTx(ctx, tx, key)
		if err != nil {
			return err
		}
		if materialized.Fence.View.DestinationUID != instance {
			return errors.New("native owner projection belongs to another destination")
		}
		if err := s.validateScopedOwnedTx(ctx, tx, materialized); err != nil {
			return err
		}
	}
	return nil
}
