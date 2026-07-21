package db

import (
	"context"
	"database/sql"
)

// Transaction is the storage-neutral database/sql surface available to a
// request-scoped fence. *sql.Tx and manually managed *sql.Conn transactions
// both implement it.
type Transaction interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// TransactionFence runs after a transaction begins and before its first
// domain write. Any returned error aborts the transaction.
type TransactionFence func(context.Context, Transaction) error

type transactionFenceContextKey struct{}

// WithTransactionFence attaches one request-scoped fence to ctx.
func WithTransactionFence(ctx context.Context, fence TransactionFence) context.Context {
	if fence == nil {
		return ctx
	}
	return context.WithValue(ctx, transactionFenceContextKey{}, fence)
}

// ApplyTransactionFence invokes the request fence, if any, against tx.
func ApplyTransactionFence(ctx context.Context, tx Transaction) error {
	fence, _ := ctx.Value(transactionFenceContextKey{}).(TransactionFence)
	if fence == nil {
		return nil
	}
	return fence(ctx, tx)
}

// HasTransactionFence reports whether ctx carries a request-scoped fence.
func HasTransactionFence(ctx context.Context) bool {
	fence, _ := ctx.Value(transactionFenceContextKey{}).(TransactionFence)
	return fence != nil
}
