package githubsync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/issuesync"
)

func TestProgressFencesOldClaimsAndReturnsCopies(t *testing.T) {
	tracker := NewProgressTracker()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tracker.Begin(1, at)
	tracker.Update(1, at, "issues", 100, 0, at.Add(time.Second))
	got := tracker.Snapshot(1, at)
	require.NotNil(t, got)
	assert.Equal(t, 100, got.Completed)
	got.Completed = 999
	assert.Equal(t, 100, tracker.Snapshot(1, at).Completed)
	newer := at.Add(time.Minute)
	tracker.Begin(1, newer)
	tracker.Begin(1, at)
	tracker.Update(1, at, "parents", 999, 0, at)
	tracker.Finish(1, at)
	require.NotNil(t, tracker.Snapshot(1, newer))
	assert.Equal(t, "repository", tracker.Snapshot(1, newer).Phase)
	assert.Nil(t, tracker.Snapshot(1, at))
	tracker.Begin(2, at)
	tracker.Finish(1, newer)
	assert.Nil(t, tracker.Snapshot(1, newer))
	require.NotNil(t, tracker.Snapshot(2, at))
}

func TestProgressConcurrentSnapshots(t *testing.T) {
	tracker := NewProgressTracker()
	at := time.Now()
	tracker.Begin(1, at)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for i := range 100 {
				tracker.Update(1, at, "issues", i, 0, at)
				got := tracker.Snapshot(1, at)
				if got == nil {
					t.Error("active snapshot missing")
				}
			}
		})
	}
	wg.Wait()
}

func TestProgressReporterIsContextScoped(t *testing.T) {
	var completed int
	ctx := withProgressReporter(context.Background(), func(phase string, count, total int) {
		assert.Zero(t, total)
		assert.Equal(t, "issues", phase)
		completed = count
	})
	issuesync.ReportProgress(ctx, "issues", 20, 0)
	reportProgress(context.Background(), "issues", 999, 0)
	assert.Equal(t, 20, completed)
}
