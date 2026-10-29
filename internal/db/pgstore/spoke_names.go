package pgstore

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
	return s.withTx(ctx, sql.LevelReadCommitted, func(tx *sql.Tx) error {
		if err := lockSpokeNames(ctx, tx); err != nil {
			return err
		}
		return db.WriteFederationSpokeName(ctx, tx, uid, name)
	})
}

// Updating the existing identity row makes stale serializable snapshots retry,
// and serializes read-committed callers without a new persisted object.
func lockSpokeNames(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE meta SET value=value WHERE key='instance_uid'`)
	return err
}
