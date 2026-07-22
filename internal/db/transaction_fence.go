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

// WithAdditionalTransactionFence appends fence after any fence already carried
// by ctx. The existing fence runs first and a rejection prevents the additional
// fence and domain mutation from running.
func WithAdditionalTransactionFence(ctx context.Context, fence TransactionFence) context.Context {
	if fence == nil {
		return ctx
	}
	existing, _ := ctx.Value(transactionFenceContextKey{}).(TransactionFence)
	if existing == nil {
		return WithTransactionFence(ctx, fence)
	}
	return WithTransactionFence(ctx, func(fenceCtx context.Context, transaction Transaction) error {
		if err := existing(fenceCtx, transaction); err != nil {
			return err
		}
		return fence(fenceCtx, transaction)
	})
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
