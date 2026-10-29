package sqlitestore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

// ListFederationSpokeNames reads this hub's local display labels.
func (s *Store) ListFederationSpokeNames(ctx context.Context) (map[string]string, error) {
	return db.ReadFederationSpokeNames(ctx, s)
}

// SetFederationSpokeName atomically sets or clears a unique hub-local label.
func (s *Store) SetFederationSpokeName(ctx context.Context, uid, name string) error {
	return s.RetryTransient(ctx, func() error {
		tx, err := s.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := lockSpokeNames(ctx, tx); err != nil {
			return err
		}
		if err := db.WriteFederationSpokeName(ctx, tx, uid, name); err != nil {
			return err
		}
		return tx.Commit()
	})
}
func lockSpokeNames(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE meta SET value=value WHERE 0`)
	return err
}
