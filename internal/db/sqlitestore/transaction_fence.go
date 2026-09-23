package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// BeginTx applies a request-scoped transaction fence before returning a
// transaction to storage code. Contexts without a host fence retain the
// ordinary standalone behavior.
func (d *Store) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	tx, err := d.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts != nil && opts.ReadOnly {
		return tx, nil
	}
	if err := db.ApplyTransactionFence(ctx, tx); err != nil {
		rollbackErr := tx.Rollback()
		return nil, db.FinishTransactionRollback(ctx, err, rollbackErr)
	}
	return tx, nil
}

// ExecContext wraps otherwise-autocommit writes in a fenced transaction when
// the request carries host access state.
func (d *Store) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if !db.HasTransactionFence(ctx) {
		return d.DB.ExecContext(ctx, query, args...)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (d *Store) beginImmediateTransaction(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE TRANSACTION"); err != nil {
		return err
	}
	if err := db.ApplyTransactionFence(ctx, conn); err != nil {
		_, rollbackErr := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		// Return the connection before host cleanup needs a separate transaction.
		closeErr := conn.Close()
		return db.FinishTransactionRollback(ctx, err, errors.Join(rollbackErr, closeErr))
	}
	return nil
}
