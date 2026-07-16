package pgstore

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"go.kenn.io/kata/internal/db"
)

// ErrNotImplemented is emitted only by stubgen when db.Storage grows before
// the corresponding Postgres method is implemented. The checked-in generated
// file contains no such stubs.
var ErrNotImplemented = errors.New("pgstore: storage method not implemented")

// SQLError is the credential- and row-data-free form of a Postgres server
// error. Code and Constraint are safe structural diagnostics; Detail, Hint,
// query text, and server location are deliberately omitted.
type SQLError struct {
	Code       string
	Constraint string
	domain     error
}

func (e *SQLError) Error() string {
	if e.Constraint != "" {
		return fmt.Sprintf("postgres SQLSTATE %s constraint %s", e.Code, e.Constraint)
	}
	return fmt.Sprintf("postgres SQLSTATE %s", e.Code)
}

func (e *SQLError) Unwrap() error {
	if e.domain != nil {
		return e.domain
	}
	return nil
}

// SQLState exposes the five-character Postgres error code for classification.
func (e *SQLError) SQLState() string { return e.Code }

// IsTransient reports only errors for which retrying the complete transaction
// is safe: serialization failures, deadlocks, and explicit lock unavailability.
// Connection failures are excluded because commit outcome may be ambiguous.
func IsTransient(err error) bool {
	var state interface{ SQLState() string }
	if !errors.As(err, &state) {
		return false
	}
	switch state.SQLState() {
	case "40001", "40P01", "55P03":
		return true
	default:
		return false
	}
}

// mapSQLError converts no-row results and named constraints to Storage domain
// errors while sanitizing all other Postgres errors. Query groups supply only
// the constraints whose domain meaning they own.
func mapSQLError(err error, constraintErrors map[string]error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	var safeErr *SQLError
	if errors.As(err, &safeErr) {
		if domain := constraintErrors[safeErr.Constraint]; domain != nil {
			return &SQLError{
				Code:       safeErr.Code,
				Constraint: safeErr.Constraint,
				domain:     domain,
			}
		}
		return err
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	return &SQLError{
		Code:       pgErr.Code,
		Constraint: pgErr.ConstraintName,
		domain:     constraintErrors[pgErr.ConstraintName],
	}
}
