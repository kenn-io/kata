package issuesync

import "context"

type readOnlyReconciliationKey struct{}

// WithReadOnlyStatusReconciliation is an in-process operator recovery mode.
// It removes provider mutation authority; it is never inferred from a header.
func WithReadOnlyStatusReconciliation(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyReconciliationKey{}, true)
}
func readOnlyStatusReconciliation(ctx context.Context) bool {
	value, _ := ctx.Value(readOnlyReconciliationKey{}).(bool)
	return value
}
