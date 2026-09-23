package db

import (
	"context"
	"database/sql"
	"errors"
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

// ErrTransactionFinalizationFailed reports failed rollback or host recording.
// Callers must not misreport this as a completed authorization decision.
var ErrTransactionFinalizationFailed = errors.New("transaction finalization unavailable")

type afterRollbackError struct {
	cause  error
	finish func(context.Context) error
}

func (e *afterRollbackError) Error() string { return e.cause.Error() }
func (e *afterRollbackError) Unwrap() error { return e.cause }

// AfterTransactionRollback attaches host cleanup to a rejected transaction.
func AfterTransactionRollback(cause error, finish func(context.Context) error) error {
	if cause == nil {
		cause = errors.New("transaction fence rejected")
	}
	return &afterRollbackError{cause: cause, finish: finish}
}

// FinishTransactionRollback runs only after the store has rolled back and
// returned its connection. Callback failure never permits the rejected write.
func FinishTransactionRollback(ctx context.Context, cause, rollbackErr error) error {
	if errors.Is(rollbackErr, sql.ErrTxDone) {
		rollbackErr = nil
	}
	var finishErr error
	if rejected, ok := errors.AsType[*afterRollbackError](cause); ok && rejected.finish != nil {
		finishErr = rejected.finish(ctx)
	}
	if rollbackErr != nil || finishErr != nil {
		cause = errors.Join(ErrTransactionFinalizationFailed, cause, rollbackErr, finishErr)
	}
	if observe, _ := ctx.Value(transactionRollbackObserverKey{}).(func(error)); observe != nil {
		observe(cause)
	}
	return cause
}

type transactionRollbackObserverKey struct{}

// WithTransactionRollbackObserver observes a fence rejection only after rollback
// and host cleanup finish. The observer receives the complete finalization error.
func WithTransactionRollbackObserver(ctx context.Context, observe func(error)) context.Context {
	return context.WithValue(ctx, transactionRollbackObserverKey{}, observe)
}

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

// WithIssueScopeTargets records the issues authorized or exposed by one request.
// They are rechecked before domain writes and before sending hydrated responses.
func WithIssueScopeTargets(ctx context.Context) context.Context {
	return context.WithValue(ctx, issueScopeTargetsKey{}, make(map[int64]struct{}))
}

type issueScopeTargetsKey struct{}

// RecordIssueScopeTarget retains a target for transaction and response checks.
func RecordIssueScopeTarget(ctx context.Context, issueID int64) {
	if targets, ok := ctx.Value(issueScopeTargetsKey{}).(map[int64]struct{}); ok {
		targets[issueID] = struct{}{}
	}
}

// IssueScopeTargets returns the targets admitted so far by this request.
func IssueScopeTargets(ctx context.Context) []int64 {
	if targets, ok := ctx.Value(issueScopeTargetsKey{}).(map[int64]struct{}); ok {
		ids := make([]int64, 0, len(targets))
		for id := range targets {
			ids = append(ids, id)
		}
		return ids
	}
	return nil
}
