package issuesync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type denyNamedCoordinator struct {
	calls  int
	guard  db.IssueSyncImportGuard
	target string
}

func (c *denyNamedCoordinator) WriteNamedStatus(_ context.Context, _ db.IssueSyncBinding, _ db.IssueStatusMapping, target string, guard db.IssueSyncImportGuard, _ NamedStatusSend, _ func() error) (StatusObservation, error) {
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
