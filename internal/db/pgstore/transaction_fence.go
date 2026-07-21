package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

// BeginTx applies a request-scoped transaction fence before returning a
// transaction to storage code. Contexts without a host fence retain the
// ordinary standalone behavior.
func (s *Store) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	tx, err := s.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts != nil && opts.ReadOnly {
		return tx, nil
	}
	if err := db.ApplyTransactionFence(ctx, tx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// ExecContext wraps otherwise-autocommit writes in a fenced transaction when
// the request carries host access state.
func (s *Store) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if !db.HasTransactionFence(ctx) {
		return s.DB.ExecContext(ctx, query, args...)
	}
	tx, err := s.BeginTx(ctx, nil)
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

func fencedQueryRow[T any](
	ctx context.Context,
	s *Store,
	scan func(rowScanner) (T, error),
	query string,
	args ...any,
) (T, error) {
	if !db.HasTransactionFence(ctx) {
		return scan(s.QueryRowContext(ctx, query, args...))
	}
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		var zero T
		return zero, err
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scan(tx.QueryRowContext(ctx, query, args...))
	if err != nil {
		var zero T
		return zero, err
	}
	if err := tx.Commit(); err != nil {
		var zero T
		return zero, err
	}
	return value, nil
}
