package hooks

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
)

// Sink is the minimal interface mutation handlers depend on. cfg.Hooks
// has type Sink so the daemon can swap in a Noop in tests / disabled
// builds without exposing the full Dispatcher surface.
type Sink interface {
	Enqueue(evt db.Event)
}

// ActivityLease retains the dispatcher-facing lease name while sharing the
// common finite-work contract with other daemon workers.
type ActivityLease = activity.Lease

// AcquireActivity admits finite dispatcher work.
type AcquireActivity = activity.Admission

// DispatcherDeps wires the dispatcher to its environment. Resolvers
// are pluggable so tests can inject deterministic snapshots without
// exercising the DB.
type DispatcherDeps struct {
	DBHash          string
	KataHome        string
	DaemonLog       *log.Logger
	AliasResolver   func(ctx context.Context, evt db.Event) (AliasSnapshot, bool, error)
	IssueResolver   func(ctx context.Context, issueID int64) (IssueSnapshot, error)
	CommentResolver func(ctx context.Context, commentID int64) (CommentSnapshot, error)
	ProjectResolver func(ctx context.Context, projectID int64) (ProjectSnapshot, error)
	Now             func() time.Time
	GraceWindow     time.Duration
	AcquireDrain    AcquireActivity
	Environment     []string
}

// Dispatcher is the live hook runtime. BeginProducerDrain is a daemon-lifecycle
// handoff used before Shutdown; unexported state is held by-value (no need for
// a separate state struct).
type Dispatcher struct {
	deps                     DispatcherDeps
	cfg                      Config
	queue                    chan queuedHookJob
	done                     chan struct{}
	waited                   chan struct{} // closed when wg.Wait returns; shared by all Shutdown callers
	stopped                  atomic.Bool
	producerDrain            atomic.Bool
	abortOnce                sync.Once
	enqueueMu                sync.RWMutex
	wg                       sync.WaitGroup
	snapshot                 atomic.Pointer[Snapshot]
	dropped                  atomic.Int64
	lastFullLog              atomic.Int64 // unix nanos of last hook_queue_full log
	lastWorkingDirMissingLog atomic.Int64 // unix nanos of last working_dir_missing log
	appender                 *runsAppender
	pruner                   *pruner
	outputDir                string
	inflight                 atomic.Int32
	// active maps groupKey -> struct{} for runs whose .out/.err are
	// still being written. The pruner consults this via isActive so a
	// finishing job's pruner.AddRun never unlinks a peer worker's
	// in-progress capture.
	active sync.Map
}

type queuedHookJob struct {
	HookJob
	lease *ActivityLease
}

// noopSink is the unexported implementation of Sink returned by
// NewNoop. It records nothing and never panics.
type noopSink struct{}

func (noopSink) Enqueue(_ db.Event) {}

// NewNoop returns a Sink whose Enqueue is a no-op. Used by tests that
// don't want to wire a full dispatcher and by builds where hooks are
// disabled.
func NewNoop() Sink { return noopSink{} }

// New constructs a Dispatcher. It MkdirAll's the hook root + output
// directories under deps.KataHome, seeds the prune running total via
// WalkDir, opens the runs.jsonl file for append, and starts the worker
// pool.
func New(loaded LoadedConfig, deps DispatcherDeps) (*Dispatcher, error) {
	deps = applyDispatcherDefaults(deps)
	root := filepath.Join(deps.KataHome, "hooks", deps.DBHash)
	outputDir := filepath.Join(root, "output")
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir hook output dir: %w", err)
	}
	runsPath := filepath.Join(root, "runs.jsonl")
	app, err := newRunsAppender(runsPath, loaded.Config.RunsLogMaxBytes, loaded.Config.RunsLogKeep)
	if err != nil {
		return nil, fmt.Errorf("open runs.jsonl: %w", err)
	}
	pr := newPruner(outputDir, loaded.Config.OutputDiskCap, deps.DaemonLog)
	if err := pr.Seed(); err != nil {
		_ = app.Close()
		return nil, fmt.Errorf("seed pruner: %w", err)
	}
	d := &Dispatcher{
		deps:      deps,
		cfg:       loaded.Config,
		queue:     make(chan queuedHookJob, loaded.Config.QueueCap),
		done:      make(chan struct{}),
		waited:    make(chan struct{}),
		appender:  app,
		pruner:    pr,
		outputDir: outputDir,
	}
	pr.SetActiveCheck(d.isGroupActive)
	snap := loaded.Snapshot
	d.snapshot.Store(&snap)
	for i := 0; i < loaded.Config.PoolSize; i++ {
		d.wg.Add(1)
		go d.worker()
	}
	return d, nil
}

// isGroupActive is the predicate the pruner consults to skip
// in-flight (event_id, hook_index) groups during a sweep.
func (d *Dispatcher) isGroupActive(k groupKey) bool {
	_, ok := d.active.Load(k)
	return ok
}

func applyDispatcherDefaults(deps DispatcherDeps) DispatcherDeps {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.GraceWindow == 0 {
		deps.GraceWindow = 5 * time.Second
	}
	if deps.DaemonLog == nil {
		deps.DaemonLog = log.New(os.Stderr, "", log.LstdFlags)
	}
	if deps.Environment == nil {
		deps.Environment = os.Environ()
	} else {
		deps.Environment = append([]string(nil), deps.Environment...)
	}
	return deps
}

// Enqueue is the Sink-side fan-out point. Non-blocking by contract: a
// full queue increments dropped and rate-limit-logs.
func (d *Dispatcher) Enqueue(evt db.Event) {
	d.enqueue(evt, d.deps.AcquireDrain)
}

// EnqueueFrom uses a parent operation's fork source for jobs caused by
// already-admitted background work. This preserves delivery after the idle
// deadline blocks unrelated top-level drains.
func (d *Dispatcher) EnqueueFrom(evt db.Event, acquire AcquireActivity) {
	d.enqueue(evt, acquire)
}

// BeginProducerDrain keeps the dispatcher open for handoffs from already
// admitted producers after daemon-wide idle admission closes. Jobs accepted in
// this phase need no idle lease because the shutdown coordinator joins the
// dispatcher explicitly after every producer exits.
func (d *Dispatcher) BeginProducerDrain() {
	d.producerDrain.Store(true)
}

// EnqueueFrom routes an event through a parent activity source when the sink
// supports it, falling back to the ordinary Sink contract for lightweight
// test and disabled implementations.
func EnqueueFrom(sink Sink, evt db.Event, acquire AcquireActivity) {
	if acquire == nil {
		sink.Enqueue(evt)
		return
	}
	if aware, ok := sink.(interface {
		EnqueueFrom(db.Event, AcquireActivity)
	}); ok {
		aware.EnqueueFrom(evt, acquire)
		return
	}
	sink.Enqueue(evt)
}

func (d *Dispatcher) enqueue(evt db.Event, acquire AcquireActivity) {
	if d.stopped.Load() {
		return
	}
	d.enqueueMu.RLock()
	defer d.enqueueMu.RUnlock()
	if d.stopped.Load() {
		return
	}
	snap := d.snapshot.Load()
	if snap == nil {
		return
	}
	for _, h := range snap.Hooks {
		if !h.Match(evt.Type) {
			continue
		}
		// Two-phase send: bail out if Shutdown started between hooks.
		select {
		case <-d.done:
			return
		default:
		}
		var lease *ActivityLease
		if acquire != nil {
			// acquire may decide the daemon is idle and invoke the shutdown
			// trigger while enqueueMu is read-locked. That trigger must not
			// call Shutdown synchronously, because Shutdown takes the write
			// lock; it only seals admission and cancels the root context.
			var admitted bool
			lease, admitted = acquire()
			if !admitted {
				if !d.producerDrain.Load() {
					d.dropped.Add(1)
					d.maybeLogAdmissionDenied()
					continue
				}
				lease = nil
			}
		}
		job := queuedHookJob{
			Event: evt, Hook: h, EnqueuedAt: d.deps.Now(),
			lease: lease,
		}
		select {
		case d.queue <- job:
		default:
			releaseActivity(lease)
			d.dropped.Add(1)
			d.maybeLogQueueFull()
		}
	}
}

func (d *Dispatcher) maybeLogQueueFull() {
	d.maybeLogDrop("hook_queue_full")
}

func (d *Dispatcher) maybeLogAdmissionDenied() {
	d.maybeLogDrop("hook_admission_denied")
}

// maybeLogDrop prints one rate-limited line per QueueFullLogInterval for any
// reason a matching hook job was not accepted.
func (d *Dispatcher) maybeLogDrop(reason string) {
	now := d.deps.Now().UnixNano()
	last := d.lastFullLog.Load()
	if now-last < int64(d.cfg.QueueFullLogInterval) {
		return
	}
	if d.lastFullLog.CompareAndSwap(last, now) {
		d.deps.DaemonLog.Printf("hooks: %s (dropped=%d)", reason, d.dropped.Load())
	}
}

// maybeLogWorkingDirMissing prints one rate-limited line when a hook's
// working_dir is missing at fire time. Reuses QueueFullLogInterval as
// the throttle so a single tunable governs every spammy hook warning.
// The runRecord still captures every occurrence; this log is purely
// for operator visibility (master spec §8.8).
func (d *Dispatcher) maybeLogWorkingDirMissing(h ResolvedHook) {
	now := d.deps.Now().UnixNano()
	last := d.lastWorkingDirMissingLog.Load()
	if now-last < int64(d.cfg.QueueFullLogInterval) {
		return
	}
	if d.lastWorkingDirMissingLog.CompareAndSwap(last, now) {
		d.deps.DaemonLog.Printf("hooks: working_dir_missing for hook[%d] %q", h.Index, h.WorkingDir)
	}
}

// CurrentConfig returns the active startup-only Config. Used by
// LoadReload to compute the UnchangedTunables diff.
func (d *Dispatcher) CurrentConfig() Config { return d.cfg }

// Reload swaps the live Snapshot. Tunables are not applied (v1: startup
// only); UnchangedTunables messages are logged.
func (d *Dispatcher) Reload(loaded LoadedConfig) {
	for _, msg := range loaded.UnchangedTunables {
		d.deps.DaemonLog.Printf("hooks: %s", msg)
	}
	snap := loaded.Snapshot
	d.snapshot.Store(&snap)
	d.deps.DaemonLog.Printf("hooks reload ok: %d hook(s) active", len(loaded.Snapshot.Hooks))
}

// Shutdown seals the queue, drains accepted jobs, and waits for workers
// before closing the runs appender. When ctx carries a deadline, in-flight
// work is cancelled early enough that killTreeWithGrace can finish inside the
// budget, so a slow hook still yields a clean join. If workers are still
// running when ctx expires, Shutdown returns an error and leaves the appender
// open. Later calls share the same `waited` channel so a retry after the first
// call timed out blocks until completion (or its own ctx expires) — never
// spuriously returns nil while workers are still running.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	if d.stopped.CompareAndSwap(false, true) {
		d.enqueueMu.Lock()
		close(d.queue)
		d.enqueueMu.Unlock()
		go func() {
			d.wg.Wait()
			close(d.waited)
		}()
	}
	abort, stopAbort := d.inFlightAbortSignal(ctx)
	defer stopAbort()
	select {
	case <-d.waited:
		return d.finishShutdown()
	case <-abort:
		d.abortInFlight("deadline approaching")
		select {
		case <-d.waited:
			return d.finishShutdown()
		case <-ctx.Done():
		}
	case <-ctx.Done():
		d.abortInFlight("budget exhausted")
	}
	d.deps.DaemonLog.Printf("hooks shutdown timed out: %d in-flight", d.inflight.Load())
	// Drain best-effort: don't close appender — workers may still
	// be writing. Caller proceeds with daemon shutdown anyway.
	return fmt.Errorf("hooks shutdown timed out: %w", ctx.Err())
}

// inFlightAbortSignal fires far enough before ctx's deadline that a cancelled
// hook can be terminated, killed after GraceWindow, and reaped inside the
// remaining budget. Without a deadline it never fires.
func (d *Dispatcher) inFlightAbortSignal(ctx context.Context) (<-chan time.Time, func()) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, func() {}
	}
	delay := max(time.Until(deadline)-2*d.deps.GraceWindow, 0)
	timer := time.NewTimer(delay)
	return timer.C, func() { timer.Stop() }
}

func (d *Dispatcher) abortInFlight(reason string) {
	d.abortOnce.Do(func() {
		close(d.done)
		d.deps.DaemonLog.Printf("hooks shutdown: cancelling %d in-flight, dropping %d queued (%s)",
			d.inflight.Load(), len(d.queue), reason)
	})
	d.releaseQueued()
}

func (d *Dispatcher) finishShutdown() error {
	d.releaseQueued()
	// Close appender once; appender.Close is itself idempotent.
	_ = d.appender.Close()
	return nil
}

// worker pops one HookJob at a time and runs it. Graceful shutdown closes the
// queue so workers drain every accepted job. Forced shutdown closes done; the
// post-pop check then drops at most one just-popped job per worker.
func (d *Dispatcher) worker() {
	defer d.wg.Done()
	rd := d.runDeps()
	for {
		select {
		case <-d.done:
			return
		case job, ok := <-d.queue:
			if !ok {
				return
			}
			select {
			case <-d.done:
				releaseActivity(job.lease)
				return // drop the just-popped job; do not start runJob
			default:
			}
			d.runOne(rd, job)
		}
	}
}

// runOne executes one job under inflight + active-group accounting
// that survives a runJob panic. The deferred recover keeps the worker
// alive and the counters accurate so Shutdown's "in-flight" log
// doesn't drift, and so the pruner's isActive predicate never strands
// a stale "still writing" entry.
func (d *Dispatcher) runOne(rd runDeps, queued queuedHookJob) {
	defer releaseActivity(queued.lease)
	job := queued.HookJob
	key := groupKey{eventID: job.Event.ID, hookIndex: job.Hook.Index}
	d.active.Store(key, struct{}{})
	d.inflight.Add(1)
	defer func() {
		d.inflight.Add(-1)
		d.active.Delete(key)
	}()
	defer func() {
		if r := recover(); r != nil {
			d.deps.DaemonLog.Printf("hooks: runJob panic: %v", r)
		}
	}()
	ctx, cancel := contextFromShutdown(d.done)
	defer cancel()
	runJob(ctx, d.done, job, rd)
}

func releaseActivity(lease *ActivityLease) {
	if lease != nil {
		lease.Release()
	}
}

func (d *Dispatcher) releaseQueued() {
	for {
		select {
		case job, ok := <-d.queue:
			if !ok {
				return
			}
			releaseActivity(job.lease)
		default:
			return
		}
	}
}

// contextFromShutdown returns a context that is canceled the moment
// d.done is closed. Resolver calls inside runJob (project, issue,
// comment, alias) honor this context, so a stuck DB query during
// stdin payload assembly does not delay Shutdown beyond ctx.
func contextFromShutdown(done <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// runDeps builds the per-job runDeps from the dispatcher's state.
// Pulled out of worker so the worker body stays under CC 8.
func (d *Dispatcher) runDeps() runDeps {
	return runDeps{
		OutputDir:   d.outputDir,
		DaemonLog:   d.deps.DaemonLog,
		Now:         d.deps.Now,
		GraceWindow: d.deps.GraceWindow,
		Environment: append([]string(nil), d.deps.Environment...),
		Project:     d.deps.ProjectResolver,
		Issue:       d.deps.IssueResolver,
		Comment:     d.deps.CommentResolver,
		Alias:       d.deps.AliasResolver,
		AppendRun: func(r runRecord) {
			// Drop the active marker BEFORE asking the pruner whether to
			// sweep so MaybeSweep can prune the just-finished group too.
			// The deferred cleanup in runOne acts as a safety net for panic
			// paths that bypass AppendRun.
			d.active.Delete(groupKey{eventID: r.EventID, hookIndex: r.HookIndex})
			d.appender.Append(r)
			d.pruner.AddRun(r.EventID, r.HookIndex, r.StdoutBytes, r.StderrBytes)
		},
		LogWorkingDirMissing: d.maybeLogWorkingDirMissing,
	}
}
