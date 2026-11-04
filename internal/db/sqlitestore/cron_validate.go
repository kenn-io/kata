package sqlitestore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

func (d *Store) readCronTransaction(ctx context.Context, run func(*sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
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
func (d *Store) ValidateCronJob(ctx context.Context, in db.CronValidationInput) (db.CronValidation, error) {
	return d.cronSQL().Validate(ctx, in, true)
}

// ValidateCronWorkflow checks a definition in a read-only snapshot without saving.
func (d *Store) ValidateCronWorkflow(ctx context.Context, in db.CronValidationInput) (db.CronValidation, error) {
	return d.cronSQL().Validate(ctx, in, false)
}
