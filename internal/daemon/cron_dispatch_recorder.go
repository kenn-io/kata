package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonlog"
)

type cronDispatchKey struct {
	projectID int64
	jobUID    string
}
type cronDispatchObservation struct {
	decision api.CronDispatchDecision
	nonIdle  *api.CronDispatchDecision
	seen     bool
}

type cronDispatchLog struct {
	Version    int    `json:"version"`
	Kind       string `json:"kind"`
	ProjectUID string `json:"project_uid"`
	JobUID     string `json:"job_uid"`
	api.CronDispatchDecision
}

// CronDispatchRecorder retains committed decisions and best-effort local history.
// Its cycle spans enumeration pages; only a successful final page prunes state.
type CronDispatchRecorder struct {
	// logMu serializes records and Close without holding observation reads behind I/O.
	logMu    sync.Mutex
	mu       sync.Mutex
	last     map[cronDispatchKey]cronDispatchObservation
	appender *jsonlog.Appender
	health   api.CronHealth
	// pathErr reports an unresolvable log home on every attempted record.
	pathErr error
}

// NewCronDispatchRecorder opens its log lazily so startup tolerates log failures.
func NewCronDispatchRecorder(path string) *CronDispatchRecorder {
	return &CronDispatchRecorder{last: make(map[cronDispatchKey]cronDispatchObservation), health: api.CronHealth{LogPath: path}}
}

// ResolveCronDispatchRecorder accepts a log path lookup result so an
// unavailable home becomes an observable log failure instead of a startup failure.
func ResolveCronDispatchRecorder(path string, err error) *CronDispatchRecorder {
	r := NewCronDispatchRecorder(path)
	if err != nil {
		r.health.LogPath, r.pathErr = "", fmt.Errorf("cron dispatch log path: %w", err)
	}
	return r
}

// Record accepts committed batches or failures, never tentative planner output.
// changed controls recurring error alerts; active deliveries and skips also log.
func (r *CronDispatchRecorder) Record(ctx context.Context, c db.RecurringCronCandidate, out db.DispatchRecurringCronOut, dispatchErr error, now time.Time) (bool, error) {
	// Keep committed counts even if the caller was canceled after commit.
	// Shutdown failures do not create error or log-failure observations.
	if ctx.Err() != nil && dispatchErr != nil {
		return false, nil
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	r.mu.Lock()
	d := api.CronDispatchDecision{At: now.UTC(), DefinitionEventUID: c.ExpectedEventUID, Reason: out.Reason, Delivered: out.Delivered, More: out.More, Skipped: out.Skipped}
	if dispatchErr != nil {
		d.Delivered, d.More, d.Skipped = 0, false, cron.RecurringSkip{}
		d.Detail = dispatchErr.Error()
		if d.Reason == "" {
			d.Reason = cron.DispatchError
		}
		r.health.ErrorsSinceStart++
	}
	key := cronDispatchKey{c.ProjectID, c.JobUID}
	prev, exists := r.last[key]
	changed := !exists || prev.decision.Reason != d.Reason || prev.decision.DefinitionEventUID != d.DefinitionEventUID || prev.decision.Detail != d.Detail
	nonIdle := prev.nonIdle
	if d.Reason != cron.DispatchIdle {
		nonIdle = &d
	}
	r.last[key] = cronDispatchObservation{decision: d, nonIdle: nonIdle, seen: true}
	r.health.DeliveredSinceStart += int64(d.Delivered)
	r.health.SkippedSinceStart += int64(d.Skipped.Count)
	r.health.SkippedCountIsLowerBound = r.health.SkippedCountIsLowerBound || d.Skipped.Capped
	if !changed && d.Delivered == 0 && d.Skipped.Count == 0 && !r.health.LogErrorPresent {
		r.mu.Unlock()
		return false, nil
	}
	logPath := r.health.LogPath
	r.mu.Unlock()
	kind := "decision"
	if d.Skipped.Count > 0 {
		kind = "skipped"
	}
	err := r.appendLocked(ctx, logPath, cronDispatchLog{1, kind, c.ProjectUID, c.JobUID, d})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health.LogErrorPresent = err != nil
	if err != nil {
		r.health.LogErrorsSinceStart++
	}
	return changed, err
}

// appendLocked opens the log on demand; callers hold logMu.
func (r *CronDispatchRecorder) appendLocked(ctx context.Context, logPath string, record cronDispatchLog) error {
	if r.pathErr != nil {
		return r.pathErr
	}
	if r.appender == nil {
		appender, err := jsonlog.Open(ctx, logPath, 50<<20, 5)
		if err != nil {
			return err
		}
		r.appender = appender
	}
	return r.appender.Append(ctx, record)
}

// FinishCycle records sweep timing. Failed enumeration resets seen flags but
// retains observations; callers must restart pagination after that failure.
func (r *CronDispatchRecorder) FinishCycle(now time.Time, elapsed time.Duration, complete, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health.LastSweepAt, r.health.LastSweepMS = now.UTC(), elapsed.Milliseconds()
	if failed {
		r.health.EnumerationErrorsSinceStart++
	} else {
		r.health.LastEnumerationAt = now.UTC()
	}
	if !complete && !failed {
		return
	}
	for key, obs := range r.last {
		if complete && !failed && !obs.seen {
			delete(r.last, key)
		} else {
			obs.seen = false
			r.last[key] = obs
		}
	}
}

// Health returns candidate aggregates after the first sweep.
func (r *CronDispatchRecorder) Health() *api.CronHealth {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.health.LastSweepAt.IsZero() {
		return nil
	}
	h := r.health
	h.Jobs, h.ByReason = len(r.last), make(map[string]int)
	for _, obs := range r.last {
		h.ByReason[string(obs.decision.Reason)]++
	}
	return &h
}

// Last returns project-scoped copies of the latest batch and non-idle batch.
func (r *CronDispatchRecorder) Last(projectID int64, jobUID string) (*api.CronDispatchDecision, *api.CronDispatchDecision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	obs, ok := r.last[cronDispatchKey{projectID, jobUID}]
	if !ok {
		return nil, nil
	}
	return api.PublicCronDispatchDecision(&obs.decision), api.PublicCronDispatchDecision(obs.nonIdle)
}

// Close releases the log after the sweeper has stopped.
func (r *CronDispatchRecorder) Close() error {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	if r.appender == nil {
		return nil
	}
	return r.appender.Close()
}
