package issuesync

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"testing"
	"time"
)

type denyNamedCoordinator struct {
	calls  int
	guard  db.IssueSyncImportGuard
	target string
}

func (c *denyNamedCoordinator) WriteNamedStatus(_ context.Context, _ db.IssueSyncBinding, _ db.IssueStatusMapping, target string, guard db.IssueSyncImportGuard, _ NamedStatusSend, _ func(context.Context) error) (StatusObservation, error) {
	c.calls++
	c.guard = guard
	c.target = target
	return StatusObservation{}, db.ErrTrackerOutcomeUnknown
}

type forbiddenNamedStatusRun struct{ writes int }

func (r *forbiddenNamedStatusRun) ReadStatus(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
	return StatusObservation{}, nil
}
func (r *forbiddenNamedStatusRun) WriteStatus(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error) {
	r.writes++
	return StatusObservation{}, nil
}

// Exercise the actual pending-event service path: missing/unresolved durable
// admission must stop the provider call, retaining the original run's claim.
func TestNamedPendingStatusNeverBypassesDurableCoordinator(t *testing.T) {
	marker := "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	run := &forbiddenNamedStatusRun{}
	guard := db.IssueSyncImportGuard{BindingID: 7, Provider: "notion", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	mapping := db.IssueStatusMapping{PendingEvent: &db.Event{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Type: "issue.closed"}}
	pass := &statusPass{statusCtx: t.Context(), binding: db.IssueSyncBinding{ConnectionUID: &marker}, guard: guard, run: run}
	require.ErrorIs(t, pass.service(mapping, true), db.ErrTrackerOutcomeUnknown)
	require.Zero(t, run.writes)
	coordinator := &denyNamedCoordinator{}
	pass.statusCtx = WithNamedStatusCoordinator(t.Context(), coordinator)
	require.ErrorIs(t, pass.service(mapping, true), db.ErrTrackerOutcomeUnknown)
	require.Zero(t, run.writes)
	require.Equal(t, 1, coordinator.calls)
	require.Equal(t, guard, coordinator.guard)
	require.Equal(t, "closed", coordinator.target)
}

// A remote native admission can have a shorter lifetime than the status pass.
// Its mapping read must observe cancellation without canceling the whole pass.
type blockingTrackerAdmissionReader struct {
	db.IssueStatusReader
	entered chan context.Context
}

func (r *blockingTrackerAdmissionReader) IssueStatusMappingByID(ctx context.Context, _ db.IssueSyncImportGuard, _ int64) (db.IssueStatusMapping, error) {
	r.entered <- ctx
	<-ctx.Done()
	return db.IssueStatusMapping{}, ctx.Err()
}
func TestNamedAdmissionMappingReadUsesStageContext(t *testing.T) {
	now := time.Now()
	reader := &blockingTrackerAdmissionReader{entered: make(chan context.Context, 1)}
	pass := &statusPass{r: NewRunner(RunnerConfig{Clock: func() time.Time { return now }}), statusCtx: t.Context(), reader: reader, startedAt: now}
	stage, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pass.admissionWithContext(db.IssueStatusMapping{})(stage) }()
	received := <-reader.entered
	require.Equal(t, stage, received)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.NoError(t, pass.statusCtx.Err())
}
