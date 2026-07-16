package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type transactionFunc func(*sql.Tx) error

func (s *Store) withSerializableTx(ctx context.Context, operation transactionFunc) error {
	return s.withTx(ctx, sql.LevelSerializable, operation)
}

func (s *Store) withRepeatableReadTx(ctx context.Context, operation transactionFunc) error {
	return s.withTx(ctx, sql.LevelRepeatableRead, operation)
}

// withTx retries the complete transaction, never an individual statement.
// Each failed attempt is rolled back before RetryTransient can begin another.
func (s *Store) withTx(ctx context.Context, isolation sql.IsolationLevel, operation transactionFunc) error {
	if operation == nil {
		return errors.New("postgres transaction operation is required")
	}
	err := s.RetryTransient(ctx, func() error {
		tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: isolation})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := operation(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
	return mapSQLError(err, nil)
}

// reserveIdentityValue advances one identity sequence and returns the reserved
// value. The table and column are restricted identifiers and the sequence is
// discovered from Postgres metadata, so callers never interpolate a sequence
// name or depend on generated naming conventions.
func (s *Store) reserveIdentityValue(
	ctx context.Context,
	tx *sql.Tx,
	table string,
	column string,
) (int64, error) {
	if !schemaNamePattern.MatchString(table) || !schemaNamePattern.MatchString(column) {
		return 0, errors.New("invalid postgres identity table or column")
	}
	if tx == nil {
		return 0, errors.New("postgres identity reservation requires a transaction")
	}
	qualifiedTable := quoteIdentifier(s.schema) + "." + quoteIdentifier(table)
	var sequence sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT pg_get_serial_sequence($1, $2)`, qualifiedTable, column).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("resolve identity sequence for %s.%s: %w", table, column, err)
	}
	if !sequence.Valid || sequence.String == "" {
		return 0, fmt.Errorf("identity sequence for %s.%s not found", table, column)
	}
	var value int64
	if err := tx.QueryRowContext(ctx, `SELECT nextval($1::regclass)`, sequence.String).Scan(&value); err != nil {
		return 0, fmt.Errorf("reserve identity value for %s.%s: %w", table, column, err)
	}
	return value, nil
}
