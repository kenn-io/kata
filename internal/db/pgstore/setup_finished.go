package pgstore

import (
	"context"
	"database/sql"
	"time"

	"go.kenn.io/kata/internal/db"
)

// PrepareSetupFinished saves an identity before any network request.
func (s *Store) PrepareSetupFinished(ctx context.Context, observed time.Time) (pending *db.SetupFinishedDelivery, complete bool, err error) {
	err = s.setupFinishedTx(ctx, func(tx *sql.Tx, uid string) error {
		var err error
		pending, complete, err = db.PrepareSetupFinishedTx(ctx, tx, uid, observed)
		return err
	})
	return pending, complete, err
}

// CompleteSetupFinished acknowledges only the saved delivery for the current install.
func (s *Store) CompleteSetupFinished(ctx context.Context, delivery db.SetupFinishedDelivery) error {
	return s.setupFinishedTx(ctx, func(tx *sql.Tx, uid string) error {
		return db.CompleteSetupFinishedTx(ctx, tx, uid, delivery)
	})
}

func (s *Store) setupFinishedTx(ctx context.Context, run func(*sql.Tx, string) error) error {
	return s.withTx(ctx, sql.LevelDefault, func(tx *sql.Tx) error {
		var uid string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance_uid' FOR UPDATE`).Scan(&uid); err != nil {
			return err
		}
		return run(tx, uid)
	})
}
