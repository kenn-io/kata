package issuesync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Replacing the ownership checks lets old claims corrupt a successor's progress.
func TestProgressClaimOwnership(t *testing.T) {
	tracker := NewProgressTracker()
	at := time.Date(2026, 1, 1, 0, 0, 0, 123000000, time.UTC)
	tracker.BeginWithPhase(1, at, "source")
	require.Equal(t, "source", tracker.Snapshot(1, at).Phase)
	require.Nil(t, tracker.Snapshot(1, at.Add(time.Nanosecond)))
	successor := at.Add(time.Minute)
	tracker.BeginWithPhase(1, successor, "source")
	tracker.Begin(1, at)
	tracker.Update(1, at, "pages", 999, 0, at)
	tracker.Finish(1, at)
	got := tracker.Snapshot(1, successor)
	require.NotNil(t, got)
	require.Equal(t, "source", got.Phase)
	got.Completed = 999
	require.Zero(t, tracker.Snapshot(1, successor).Completed)
	require.Nil(t, tracker.Snapshot(1, at))
	tracker.Finish(1, successor)
	require.Nil(t, tracker.Snapshot(1, successor))
}

func TestProgressReporterIsContextScoped(t *testing.T) {
	var completed int
	ctx := WithProgressReporter(context.Background(), func(phase string, count, total int) {
		require.Equal(t, "pages", phase)
		require.Zero(t, total)
		completed = count
	})
	ReportProgress(ctx, "pages", 20, 0)
	ReportProgress(context.Background(), "pages", 999, 0)
	require.Equal(t, 20, completed)
}
