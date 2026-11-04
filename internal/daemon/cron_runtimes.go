package daemon

import (
	"errors"
	"sync"
	"time"

	"go.kenn.io/kata/internal/cron"
)

var errCronRuntimeLimit = errors.New("cron runtime inventory is full")

type cronRuntimeKey struct {
	projectID int64
	uid       string
}

// CronRuntimeRegistry belongs to one server lifetime, independent of persisted events.
// It retains at most 256 entries per project and 4096 across the daemon.
type CronRuntimeRegistry struct {
	mu        sync.Mutex
	entries   map[cronRuntimeKey]cron.Runtime
	startedAt time.Time
	clock     func() time.Time
}

// NewCronRuntimeRegistry uses an injected clock for deterministic expiry tests.
func NewCronRuntimeRegistry(startedAt time.Time, clock func() time.Time) *CronRuntimeRegistry {
	if clock == nil {
		clock = time.Now
	}
	if startedAt.IsZero() {
		startedAt = clock().UTC()
	}
	return &CronRuntimeRegistry{entries: map[cronRuntimeKey]cron.Runtime{}, startedAt: startedAt, clock: clock}
}
func (r *CronRuntimeRegistry) prune(now time.Time) {
	for k, v := range r.entries {
		if !now.Before(v.ExpiresAt.Add(24 * time.Hour)) {
			delete(r.entries, k)
		}
	}
}

// beatNormalized takes ownership of a beat validated by NormalizeRuntimeBeat.
func (r *CronRuntimeRegistry) beatNormalized(projectID int64, id, actor string, b cron.RuntimeBeat, forwarder string) (cron.Runtime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock().UTC()
	r.prune(now)
	key := cronRuntimeKey{projectID, id}
	old, exists := r.entries[key]
	if !exists {
		count := 0
		for k := range r.entries {
			if k.projectID == projectID {
				count++
			}
		}
		if count >= 256 || len(r.entries) >= 4096 {
			return cron.Runtime{}, errCronRuntimeLimit
		}
	}
	first := old.FirstSeenAt
	if !exists {
		first = now
	}
	v := cron.Runtime{RuntimeBeat: b, UID: id, Actor: actor, FirstSeenAt: first, LastSeenAt: now, ExpiresAt: now.Add(time.Duration(b.IntervalSeconds) * 3 * time.Second), ReportedVia: "direct"}
	if forwarder != "" {
		v.ReportedVia = "forwarded"
		v.ForwarderActor = forwarder
	}
	if b.State == "stopping" {
		v.ExpiresAt = now
	}
	v.Status = cron.RuntimeStatus(v, now)
	r.entries[key] = v
	v.RuntimeBeat = cron.CloneRuntimeBeat(v.RuntimeBeat)
	return v, nil
}

// List evaluates a coherent copy without holding the lock during job warning evaluation.
func (r *CronRuntimeRegistry) List(projectID int64, jobs []cron.RuntimeJob, daemonVersion string) cron.RuntimeInventory {
	r.mu.Lock()
	now := r.clock().UTC()
	r.prune(now)
	entries := []cron.Runtime{}
	for k, v := range r.entries {
		if k.projectID == projectID {
			entries = append(entries, v)
		}
	}
	r.mu.Unlock()
	return cron.EvaluateRuntimes(entries, jobs, now, daemonVersion, r.startedAt)
}
