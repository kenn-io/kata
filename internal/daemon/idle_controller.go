package daemon

import (
	"sync"
	"time"

	"go.kenn.io/kata/internal/activity"
)

type idleTimer interface {
	Stop() bool
}

// DrainAdmission adapts the controller to the shared finite-work contract.
func (c *IdleController) DrainAdmission() activity.Admission {
	return func() (*activity.Lease, bool) {
		lease, admitted := c.TryDrain()
		if !admitted {
			return nil, false
		}
		return activityLeaseFromIdle(lease), true
	}
}

// WaitableDrainAdmission adapts reversible blocked-expiration denials for
// long-lived schedulers that must resume if foreground activity revives the
// daemon.
func (c *IdleController) WaitableDrainAdmission() activity.WaitableAdmission {
	return func() (*activity.Lease, bool, <-chan struct{}) {
		lease, admitted, retry := c.TryDrainWaitable()
		if !admitted {
			return nil, false, retry
		}
		return activityLeaseFromIdle(lease), true, nil
	}
}

// ForegroundAdmission adapts explicit client work to the shared lease shape.
func (c *IdleController) ForegroundAdmission() activity.Admission {
	return func() (*activity.Lease, bool) {
		lease, admitted := c.TryForeground()
		if !admitted {
			return nil, false
		}
		return activity.NewLease(lease.Release, nil), true
	}
}

func activityLeaseFromIdle(lease *IdleLease) *activity.Lease {
	if lease == nil {
		return nil
	}
	return activity.NewLease(lease.Release, func() (*activity.Lease, bool) {
		child, admitted := lease.Fork()
		if !admitted {
			return nil, false
		}
		return activityLeaseFromIdle(child), true
	})
}

type idleClock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) idleTimer
}

type wallIdleClock struct{}

func (wallIdleClock) Now() time.Time {
	return time.Now()
}

func (wallIdleClock) AfterFunc(delay time.Duration, callback func()) idleTimer {
	return time.AfterFunc(delay, callback)
}

// IdleController requests daemon shutdown after a period without foreground
// activity. Call Start once daemon startup and runtime publication complete.
type IdleController struct {
	mu         sync.Mutex
	timeout    time.Duration
	onIdle     func()
	clock      idleClock
	timer      idleTimer
	deadline   time.Time
	generation uint64
	started    bool
	stopping   bool
	foreground int
	drains     int
	expired    bool
	drainRetry chan struct{}
}

// IdleState describes the controller's observable lifecycle phase.
type IdleState string

const (
	// IdleStateDisabled means idle shutdown is not active.
	IdleStateDisabled IdleState = "disabled"
	// IdleStateArmed means the daemon is awaiting its idle deadline.
	IdleStateArmed IdleState = "armed"
	// IdleStateForeground means explicit client work is active.
	IdleStateForeground IdleState = "foreground"
	// IdleStateBlocked means the deadline elapsed while drain work remains.
	IdleStateBlocked IdleState = "blocked"
	// IdleStateStopping means admission is closed and shutdown has started.
	IdleStateStopping IdleState = "stopping"
)

// IdleSnapshot is a read-only view suitable for health reporting.
type IdleSnapshot struct {
	Timeout  time.Duration
	State    IdleState
	Deadline time.Time
}

type idleLeaseKind uint8

const (
	idleLeaseForeground idleLeaseKind = iota
	idleLeaseDrain
)

// IdleLease protects admitted daemon work until Release is called.
type IdleLease struct {
	controller *IdleController
	kind       idleLeaseKind
	released   bool
	forkable   bool
}

// NewIdleController constructs a controller backed by the wall clock.
func NewIdleController(timeout time.Duration, onIdle func()) *IdleController {
	return newIdleControllerWithClock(timeout, onIdle, wallIdleClock{})
}

func newIdleControllerWithClock(
	timeout time.Duration, onIdle func(), clock idleClock,
) *IdleController {
	return &IdleController{timeout: timeout, onIdle: onIdle, clock: clock}
}

// Snapshot returns the controller's current effective state.
func (c *IdleController) Snapshot() IdleSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := IdleSnapshot{Timeout: c.timeout, Deadline: c.deadline}
	switch {
	case c.stopping:
		snapshot.State = IdleStateStopping
		snapshot.Deadline = time.Time{}
	case !c.started || c.timeout <= 0:
		snapshot.State = IdleStateDisabled
	case c.foreground > 0:
		snapshot.State = IdleStateForeground
		snapshot.Deadline = time.Time{}
	case c.expired:
		snapshot.State = IdleStateBlocked
	default:
		snapshot.State = IdleStateArmed
	}
	return snapshot
}

// TryForeground admits client activity unless shutdown has started.
func (c *IdleController) TryForeground() (*IdleLease, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping {
		return nil, false
	}
	c.foreground++
	if c.expired {
		c.wakeDrainRetryLocked()
	}
	c.expired = false
	c.generation++
	if c.timer != nil {
		c.timer.Stop()
	}
	return &IdleLease{controller: c, kind: idleLeaseForeground}, true
}

// TryDrain admits a top-level background operation before the idle deadline.
func (c *IdleController) TryDrain() (*IdleLease, bool) {
	lease, admitted, _ := c.TryDrainWaitable()
	return lease, admitted
}

// TryDrainWaitable admits finite background work and, for a reversible
// blocked-expiration denial, atomically returns a channel closed when
// foreground activity reopens admission. A nil retry channel means shutdown
// is terminal and callers should wait only for root cancellation.
func (c *IdleController) TryDrainWaitable() (*IdleLease, bool, <-chan struct{}) {
	c.mu.Lock()
	if c.stopping {
		c.mu.Unlock()
		return nil, false, nil
	}
	if c.expired {
		retry := c.drainRetryLocked()
		c.mu.Unlock()
		return nil, false, retry
	}
	if c.started && c.foreground == 0 && !c.clock.Now().Before(c.deadline) {
		c.expired = true
		shouldStop := c.drains == 0
		var retry <-chan struct{}
		if shouldStop {
			c.stopping = true
			c.generation++
			c.wakeDrainRetryLocked()
		} else {
			retry = c.drainRetryLocked()
		}
		onIdle := c.onIdle
		c.mu.Unlock()
		if shouldStop && onIdle != nil {
			onIdle()
		}
		return nil, false, retry
	}
	c.drains++
	c.mu.Unlock()
	return &IdleLease{controller: c, kind: idleLeaseDrain, forkable: true}, true, nil
}

// Release completes admitted work. It is safe to call more than once.
func (l *IdleLease) Release() {
	if l == nil {
		return
	}
	l.controller.release(l)
}

// Fork transfers protection from an admitted top-level drain operation to one
// finite child operation. Child leases cannot fork again.
func (l *IdleLease) Fork() (*IdleLease, bool) {
	if l == nil || l.controller == nil {
		return nil, false
	}
	c := l.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping || l.released || l.kind != idleLeaseDrain || !l.forkable {
		return nil, false
	}
	c.drains++
	return &IdleLease{controller: c, kind: idleLeaseDrain}, true
}

func (c *IdleController) release(lease *IdleLease) {
	c.mu.Lock()
	if lease.released {
		c.mu.Unlock()
		return
	}
	lease.released = true
	if c.stopping {
		c.mu.Unlock()
		return
	}
	switch lease.kind {
	case idleLeaseForeground:
		c.foreground--
		if c.started && c.foreground == 0 {
			c.armLocked(c.clock.Now().Add(c.timeout))
		}
		c.mu.Unlock()
	case idleLeaseDrain:
		c.drains--
		shouldStop := c.expired && c.drains == 0 && c.foreground == 0
		if shouldStop {
			c.stopping = true
			c.generation++
			c.wakeDrainRetryLocked()
		}
		onIdle := c.onIdle
		c.mu.Unlock()
		if shouldStop && onIdle != nil {
			onIdle()
		}
	default:
		c.mu.Unlock()
	}
}

// Start begins the initial idle interval. Repeated calls are harmless.
func (c *IdleController) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.stopping || c.timeout <= 0 {
		return
	}
	c.started = true
	if c.foreground > 0 {
		return
	}
	c.armLocked(c.clock.Now().Add(c.timeout))
}

// Stop disables idle shutdown. Repeated calls are harmless.
func (c *IdleController) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping {
		return
	}
	c.stopping = true
	c.generation++
	c.wakeDrainRetryLocked()
	if c.timer != nil {
		c.timer.Stop()
	}
}

func (c *IdleController) armLocked(deadline time.Time) {
	if c.expired {
		c.wakeDrainRetryLocked()
	}
	c.generation++
	generation := c.generation
	c.deadline = deadline
	c.expired = false
	if c.timer != nil {
		c.timer.Stop()
	}
	delay := deadline.Sub(c.clock.Now())
	if delay < 0 {
		delay = 0
	}
	c.timer = c.clock.AfterFunc(delay, func() {
		c.expire(generation)
	})
}

func (c *IdleController) expire(generation uint64) {
	c.mu.Lock()
	if c.stopping || generation != c.generation {
		c.mu.Unlock()
		return
	}
	if now := c.clock.Now(); now.Before(c.deadline) {
		c.armLocked(c.deadline)
		c.mu.Unlock()
		return
	}
	if c.drains > 0 {
		c.expired = true
		c.mu.Unlock()
		return
	}
	c.stopping = true
	c.generation++
	c.wakeDrainRetryLocked()
	onIdle := c.onIdle
	c.mu.Unlock()
	if onIdle != nil {
		onIdle()
	}
}

func (c *IdleController) drainRetryLocked() <-chan struct{} {
	if c.drainRetry == nil {
		c.drainRetry = make(chan struct{})
	}
	return c.drainRetry
}

func (c *IdleController) wakeDrainRetryLocked() {
	if c.drainRetry == nil {
		return
	}
	close(c.drainRetry)
	c.drainRetry = nil
}
