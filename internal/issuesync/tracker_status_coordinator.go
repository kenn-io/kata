package issuesync

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

// NamedStatusSend invokes a provider with a callback that admits the paced write.
type NamedStatusSend func(context.Context, func() error) (StatusObservation, error)

// NamedStatusCoordinator retains durable attempt/result state around one
// provider call. The callback is invoked by the provider after pacing. The
// shared engine supplies the original claim guard, never a rediscovered claim.
type NamedStatusCoordinator interface {
	WriteNamedStatus(context.Context, db.IssueSyncBinding, db.IssueStatusMapping, string, db.IssueSyncImportGuard, NamedStatusSend, func() error) (StatusObservation, error)
}

// NamedStatusRecovery acknowledges already retained completed responses before
// inspecting today's pending events. It does not perform a provider request.
type NamedStatusRecovery interface {
	RecoverNamedStatus(context.Context, db.IssueSyncBinding, db.IssueSyncImportGuard) error
}
type namedStatusCoordinatorKey struct{}

// WithNamedStatusCoordinator attaches the coordinator for this invocation.
func WithNamedStatusCoordinator(ctx context.Context, coordinator NamedStatusCoordinator) context.Context {
	return context.WithValue(ctx, namedStatusCoordinatorKey{}, coordinator)
}
func namedStatusCoordinator(ctx context.Context) (NamedStatusCoordinator, bool) {
	c, ok := ctx.Value(namedStatusCoordinatorKey{}).(NamedStatusCoordinator)
	return c, ok && c != nil
}
