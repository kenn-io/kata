// Package issuesync owns shared built-in issue sync orchestration and progress.
package issuesync

import (
	"context"
	"sync"
	"time"
)

// Progress is a live, phase-local count for one daemon-owned sync claim.
// Total is zero when the upstream cannot supply a reliable total.
type Progress struct {
	Phase     string
	Completed int
	Total     int
	StartedAt time.Time
	UpdatedAt time.Time
}

// ProgressTracker holds only active runs. It never persists progress.
type ProgressTracker struct {
	mu     sync.RWMutex
	active map[int64]Progress
}

// NewProgressTracker creates a daemon-owned registry shared by its runners.
func NewProgressTracker() *ProgressTracker { return &ProgressTracker{active: make(map[int64]Progress)} }

// Begin starts a claim without replacing a newer run that has already started.
func (t *ProgressTracker) Begin(id int64, at time.Time) {
	t.BeginWithPhase(id, at, "repository")
}

// BeginWithPhase starts an owned claim at its provider-specific initial phase.
func (t *ProgressTracker) BeginWithPhase(id int64, at time.Time, phase string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.active[id]; ok && !at.After(old.StartedAt) {
		return
	}
	t.active[id] = Progress{Phase: phase, StartedAt: at, UpdatedAt: at}
}

// Update publishes completed work only for the entry owned by this claim.
func (t *ProgressTracker) Update(id int64, at time.Time, phase string, completed, total int, updatedAt time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	old, ok := t.active[id]
	if !ok || !old.StartedAt.Equal(at) {
		return
	}
	old.Phase = phase
	old.Completed = completed
	old.Total = total
	old.UpdatedAt = updatedAt
	t.active[id] = old
}

// Finish removes only the caller's claim, preserving any successor.
func (t *ProgressTracker) Finish(id int64, at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.active[id]; ok && old.StartedAt.Equal(at) {
		delete(t.active, id)
	}
}

// Snapshot returns a copy only when the persisted active claim matches.
func (t *ProgressTracker) Snapshot(id int64, at time.Time) *Progress {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	old, ok := t.active[id]
	if !ok || !old.StartedAt.Equal(at) {
		return nil
	}
	return &old
}

type progressReporterKey struct{}
type progressReporter func(string, int, int)

// WithProgressReporter attaches a reporter for this run to the context.
func WithProgressReporter(ctx context.Context, report func(string, int, int)) context.Context {
	return context.WithValue(ctx, progressReporterKey{}, progressReporter(report))
}

// ReportProgress publishes phase-local progress when a reporter is attached.
func ReportProgress(ctx context.Context, phase string, completed, total int) {
	if report, ok := ctx.Value(progressReporterKey{}).(progressReporter); ok {
		report(phase, completed, total)
	}
}
