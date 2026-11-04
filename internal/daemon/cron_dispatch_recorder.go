package daemon

import (
	"context"
	"sync"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonlog"
)

// CronDispatchDecision is an observation of a committed batch or failed attempt.
// Detail is sanitized by Last; the local JSONL record retains raw diagnostics.
type CronDispatchDecision = api.CronDispatchDecision

type cronDispatchLog struct {
	Version    int    `json:"version"`
	Kind       string `json:"kind"`
	ProjectUID string `json:"project_uid"`
	JobUID     string `json:"job_uid"`
	CronDispatchDecision
}

// CronDispatchRecorder owns daemon-local observations and a best-effort log.
// Its seen set spans enumeration pages; a failed page restarts that cycle.
type CronDispatchRecorder struct {
	mu       sync.Mutex
	last     map[string]CronDispatchDecision
	projects map[string]string
	seen     map[string]bool
	pending  map[string]bool
	appender *jsonlog.Appender
	health   api.CronHealth
}

// NewCronDispatchRecorder creates a recorder with a lazily opened local log.
func NewCronDispatchRecorder(path string) *CronDispatchRecorder {
	return &CronDispatchRecorder{last: make(map[string]CronDispatchDecision), projects: make(map[string]string), seen: make(map[string]bool), pending: make(map[string]bool), appender: jsonlog.New(path, 0, 0), health: api.CronHealth{LogPath: path}}
}

// Record must be called only after dispatch returns, never with tentative
// planner output. The bool tells the sweeper whether to repeat its error alert.
func (r *CronDispatchRecorder) Record(ctx context.Context, c db.RecurringCronCandidate, out db.DispatchRecurringCronOut, dispatchErr error, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := CronDispatchDecision{At: now.UTC(), DefinitionEventUID: c.ExpectedEventUID, Reason: out.Reason, Delivered: out.Delivered, More: out.More, Skipped: out.Skipped}
	if dispatchErr != nil {
		// Failed transactions cannot contribute tentative delivery/skip totals.
		d.Delivered, d.More, d.Skipped = 0, false, cron.RecurringSkip{}
		d.Detail = dispatchErr.Error()
		if d.Reason == "" {
			d.Reason = cron.DispatchError
		}
		r.health.ErrorsSinceStart++
	}
	previous, exists := r.last[c.JobUID]
	changed := !exists || previous.Reason != d.Reason || previous.DefinitionEventUID != d.DefinitionEventUID || previous.Detail != d.Detail
	r.last[c.JobUID], r.seen[c.JobUID], r.projects[c.JobUID] = d, true, c.ProjectUID
	r.health.DeliveredSinceStart += int64(d.Delivered)
	r.health.SkippedSinceStart += int64(d.Skipped.Count)
	r.health.SkippedCountIsLowerBound = r.health.SkippedCountIsLowerBound || d.Skipped.Capped
	if !changed && d.Delivered == 0 && d.Skipped.Count == 0 && !r.pending[c.JobUID] {
		return false, nil
	}
	kind := "decision"
	if d.Skipped.Count > 0 {
		kind = "skipped"
	}
	err := r.appender.Append(ctx, cronDispatchLog{Version: 1, Kind: kind, ProjectUID: c.ProjectUID, JobUID: c.JobUID, CronDispatchDecision: d})
	r.pending[c.JobUID] = err != nil
	r.updateLogHealth()
	if err != nil {
		r.health.LogErrorsSinceStart++
	}
	return changed, err
}

// FinishSweep prunes only on a successful full enumeration. Callers restart
// enumeration from the beginning after a listing failure.
func (r *CronDispatchRecorder) FinishSweep(now time.Time, elapsed time.Duration, complete, enumerationError bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health.LastSweepAt, r.health.LastSweepMS = now.UTC(), elapsed.Milliseconds()
	if enumerationError {
		r.health.EnumerationErrorsSinceStart++
		clear(r.seen)
		return
	}
	if !complete {
		return
	}
	at := now.UTC()
	r.health.LastEnumerationAt = &at
	for uid := range r.last {
		if !r.seen[uid] {
			delete(r.last, uid)
			delete(r.projects, uid)
			delete(r.pending, uid)
		}
	}
	clear(r.seen)
	r.updateLogHealth()
}

// updateLogHealth requires mu and reflects only retained pending appends.
func (r *CronDispatchRecorder) updateLogHealth() {
	r.health.LogErrorPresent = false
	for _, pending := range r.pending {
		r.health.LogErrorPresent = r.health.LogErrorPresent || pending
	}
}

// Health returns an aggregate snapshot after the first sweep.
func (r *CronDispatchRecorder) Health() *api.CronHealth {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.health.LastSweepAt.IsZero() {
		return nil
	}
	h := r.health
	h.Jobs, h.ByReason = len(r.last), make(map[string]int)
	for _, d := range r.last {
		h.ByReason[string(d.Reason)]++
	}
	return &h
}

// Last returns a project-scoped copy with allowlisted detail, never log contents.
func (r *CronDispatchRecorder) Last(projectUID, jobUID string) *CronDispatchDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.last[jobUID]
	if !ok || r.projects[jobUID] != projectUID {
		return nil
	}
	d.Detail = d.Reason.PublicDetail()
	return &d
}

// Close releases the local log after the sweeper has stopped.
func (r *CronDispatchRecorder) Close() error { return r.appender.Close() }
