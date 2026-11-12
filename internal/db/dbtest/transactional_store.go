package dbtest

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

// TransactionalStore preserves a wrapped store's transaction capability in tests.
type TransactionalStore struct{ db.Storage }

// BeginTx delegates transaction creation to the wrapped store.
func (s TransactionalStore) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return s.Storage.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	}).BeginTx(ctx, opts)
}
