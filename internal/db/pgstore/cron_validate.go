package pgstore

import (
	"context"
	"database/sql"
	"go.kenn.io/kata/internal/db"
)

func (s *Store) readCronTransaction(ctx context.Context, run func(*sql.Tx) error) error {
	tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := run(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ValidateCronJob checks a definition in a read-only snapshot without saving.
func (s *Store) ValidateCronJob(ctx context.Context, in db.CronValidationInput) (db.CronValidation, error) {
	return s.cronSQL().ValidateJob(ctx, in)
}

// ValidateCronWorkflow checks a definition in a read-only snapshot without saving.
func (s *Store) ValidateCronWorkflow(ctx context.Context, in db.CronValidationInput) (db.CronValidation, error) {
	return s.cronSQL().ValidateWorkflow(ctx, in)
}
