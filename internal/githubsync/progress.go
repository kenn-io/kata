package githubsync

import (
	"context"

	"go.kenn.io/kata/internal/issuesync"
)

// Progress retains the GitHub API while sharing one registry.
type Progress = issuesync.Progress

// ProgressTracker tracks live progress for GitHub sync runs.
type ProgressTracker = issuesync.ProgressTracker

// NewProgressTracker creates the shared registry used by GitHub sync.
func NewProgressTracker() *ProgressTracker { return issuesync.NewProgressTracker() }
func withProgressReporter(ctx context.Context, report func(string, int, int)) context.Context {
	return issuesync.WithProgressReporter(ctx, report)
}
func reportProgress(ctx context.Context, phase string, completed, total int) {
	issuesync.ReportProgress(ctx, phase, completed, total)
}
