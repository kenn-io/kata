package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIdleControllerStopsAfterInitialTimeout(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	idle := make(chan struct{}, 1)
	controller := newIdleControllerWithClock(time.Minute, func() {
		idle <- struct{}{}
	}, clock)

	controller.Start()
	clock.Advance(time.Minute)

	select {
	case <-idle:
	default:
		t.Fatal("idle callback was not invoked at the initial deadline")
	}
}

func TestIdleControllerForegroundActivityStartsFreshIntervalAfterRelease(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	idle := make(chan struct{}, 1)
	controller := newIdleControllerWithClock(time.Minute, func() {
		idle <- struct{}{}
	}, clock)
	controller.Start()
	clock.Advance(45 * time.Second)

	lease, admitted := controller.TryForeground()
	require.True(t, admitted)
	clock.Advance(30 * time.Second)
	requireNoIdleCallback(t, idle)

	lease.Release()
	clock.Advance(59 * time.Second)
	requireNoIdleCallback(t, idle)
	clock.Advance(time.Second)

	select {
	case <-idle:
	default:
		t.Fatal("idle callback was not invoked after the post-activity interval")
	}
}

func TestIdleControllerDrainBlocksExpiredShutdownWithoutResettingDeadline(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	idle := make(chan struct{}, 1)
	controller := newIdleControllerWithClock(time.Minute, func() {
		idle <- struct{}{}
	}, clock)
	controller.Start()

	lease, admitted := controller.TryDrain()
	require.True(t, admitted)
	clock.Advance(time.Minute)
	requireNoIdleCallback(t, idle)

	lease.Release()
	select {
	case <-idle:
	default:
		t.Fatal("idle callback was not invoked when the expired drain completed")
	}
}

func TestIdleControllerBlockedDrainCanForkOneFiniteChild(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	idle := make(chan struct{}, 1)
	controller := newIdleControllerWithClock(time.Minute, func() {
		idle <- struct{}{}
	}, clock)
	controller.Start()

	parent, admitted := controller.TryDrain()
	require.True(t, admitted)
	clock.Advance(time.Minute)

	_, admitted = controller.TryDrain()
	require.False(t, admitted, "expired controller admitted unrelated scheduled work")
	child, admitted := parent.Fork()
	require.True(t, admitted)
	_, admitted = child.Fork()
	require.False(t, admitted, "child drain lease admitted recursive derivation")

	parent.Release()
	requireNoIdleCallback(t, idle)
	child.Release()
	select {
	case <-idle:
	default:
		t.Fatal("idle callback was not invoked after the finite child completed")
	}
}

func TestIdleControllerPreservesForegroundLeaseAcquiredBeforeStart(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	idle := make(chan struct{}, 1)
	controller := newIdleControllerWithClock(time.Minute, func() {
		idle <- struct{}{}
	}, clock)

	lease, admitted := controller.TryForeground()
	require.True(t, admitted)
	controller.Start()
	clock.Advance(time.Minute)
	requireNoIdleCallback(t, idle)

	lease.Release()
	clock.Advance(time.Minute)
	select {
	case <-idle:
	default:
		t.Fatal("idle callback was not invoked after the pre-start activity completed")
	}
}

func TestIdleControllerSnapshotReportsEffectiveLifecycleState(t *testing.T) {
	start := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	clock := newIdleTestClock(start)
	controller := newIdleControllerWithClock(time.Minute, nil, clock)
	controller.Start()

	snapshot := controller.Snapshot()
	require.Equal(t, IdleStateArmed, snapshot.State)
	require.Equal(t, time.Minute, snapshot.Timeout)
	require.Equal(t, start.Add(time.Minute), snapshot.Deadline)

	drain, admitted := controller.TryDrain()
	require.True(t, admitted)
	clock.Advance(time.Minute)
	snapshot = controller.Snapshot()
	require.Equal(t, IdleStateBlocked, snapshot.State)
	require.Equal(t, start.Add(time.Minute), snapshot.Deadline)
	drain.Release()
	snapshot = controller.Snapshot()
	require.Equal(t, IdleStateStopping, snapshot.State)
	require.True(t, snapshot.Deadline.IsZero())
}

func TestIdleControllerForegroundRevivesBlockedExpiration(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	idle := make(chan struct{}, 1)
	controller := newIdleControllerWithClock(time.Minute, func() { idle <- struct{}{} }, clock)
	controller.Start()

	drain, admitted := controller.TryDrain()
	require.True(t, admitted)
	clock.Advance(time.Minute)
	require.Equal(t, IdleStateBlocked, controller.Snapshot().State)

	foreground, admitted := controller.TryForeground()
	require.True(t, admitted)
	foreground.Release()
	drain.Release()
	requireNoIdleCallback(t, idle)
	clock.Advance(time.Minute)
	select {
	case <-idle:
	default:
		t.Fatal("revived controller did not observe the fresh foreground interval")
	}
}

func TestIdleControllerWaitableDrainDenialWakesOnForegroundRevival(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	controller := newIdleControllerWithClock(time.Minute, nil, clock)
	controller.Start()

	drain, admitted := controller.TryDrain()
	require.True(t, admitted)
	clock.Advance(time.Minute)

	_, admitted, retry := controller.TryDrainWaitable()
	require.False(t, admitted)
	require.NotNil(t, retry)
	select {
	case <-retry:
		t.Fatal("blocked drain retry woke before foreground revival")
	default:
	}

	foreground, admitted := controller.TryForeground()
	require.True(t, admitted)
	select {
	case <-retry:
	case <-time.After(time.Second):
		t.Fatal("foreground revival did not wake blocked drain retry")
	}
	foreground.Release()
	drain.Release()
}

func TestIdleControllerReleasedPreStartLeaseDoesNotDelayInitialDeadline(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	idle := make(chan struct{}, 1)
	controller := newIdleControllerWithClock(time.Minute, func() { idle <- struct{}{} }, clock)
	lease, admitted := controller.TryForeground()
	require.True(t, admitted)
	lease.Release()

	controller.Start()
	clock.Advance(time.Minute)
	select {
	case <-idle:
	default:
		t.Fatal("released pre-start lease delayed the initial deadline")
	}
}

func TestIdleControllerConcurrentDuplicateReleaseIsHarmless(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	idle := make(chan struct{}, 2)
	controller := newIdleControllerWithClock(time.Minute, func() { idle <- struct{}{} }, clock)
	controller.Start()
	lease, admitted := controller.TryForeground()
	require.True(t, admitted)

	var releases sync.WaitGroup
	for range 16 {
		releases.Go(func() {
			lease.Release()
		})
	}
	releases.Wait()
	clock.Advance(time.Minute)
	require.Len(t, idle, 1)
}

func TestIdleControllerAdmissionRacingExpirationRemainsConsistent(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	idle := make(chan struct{}, 2)
	controller := newIdleControllerWithClock(time.Minute, func() { idle <- struct{}{} }, clock)
	controller.Start()
	clock.mu.Lock()
	timer := clock.timers[0]
	clock.now = clock.now.Add(time.Minute)
	clock.mu.Unlock()

	start := make(chan struct{})
	leaseResult := make(chan *IdleLease, 1)
	var racers sync.WaitGroup
	racers.Add(2)
	go func() {
		defer racers.Done()
		<-start
		timer.ForceFire()
	}()
	go func() {
		defer racers.Done()
		<-start
		lease, admitted := controller.TryForeground()
		if admitted {
			leaseResult <- lease
		}
	}()
	close(start)
	racers.Wait()
	close(leaseResult)

	if lease := <-leaseResult; lease != nil {
		requireNoIdleCallback(t, idle)
		lease.Release()
		clock.Advance(time.Minute)
	}
	require.LessOrEqual(t, len(idle), 1)
	require.Equal(t, IdleStateStopping, controller.Snapshot().State)
}

func TestIdleControllerStopRacingExpirationInvokesCallbackAtMostOnce(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	idle := make(chan struct{}, 2)
	controller := newIdleControllerWithClock(time.Minute, func() { idle <- struct{}{} }, clock)
	controller.Start()
	clock.mu.Lock()
	timer := clock.timers[0]
	clock.now = clock.now.Add(time.Minute)
	clock.mu.Unlock()

	var racers sync.WaitGroup
	racers.Add(2)
	go func() {
		defer racers.Done()
		timer.ForceFire()
	}()
	go func() {
		defer racers.Done()
		controller.Stop()
	}()
	racers.Wait()
	require.LessOrEqual(t, len(idle), 1)
	require.Equal(t, IdleStateStopping, controller.Snapshot().State)
}

func TestIdleControllerCallbackMayReenterController(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	done := make(chan struct{})
	var controller *IdleController
	controller = newIdleControllerWithClock(time.Minute, func() {
		_ = controller.Snapshot()
		controller.Stop()
		close(done)
	}, clock)
	controller.Start()
	clock.Advance(time.Minute)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle callback deadlocked while reentering the controller")
	}
}

type idleTestClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*idleTestTimer
}

func newIdleTestClock(now time.Time) *idleTestClock {
	return &idleTestClock{now: now}
}

func (c *idleTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *idleTestClock) AfterFunc(delay time.Duration, callback func()) idleTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &idleTestTimer{deadline: c.now.Add(delay), callback: callback}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *idleTestClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	now := c.now
	timers := append([]*idleTestTimer(nil), c.timers...)
	c.mu.Unlock()

	for _, timer := range timers {
		timer.FireIfDue(now)
	}
}

type idleTestTimer struct {
	mu       sync.Mutex
	deadline time.Time
	callback func()
	stopped  bool
	fired    bool
}

func (t *idleTestTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := !t.stopped && !t.fired
	t.stopped = true
	return wasActive
}

func (t *idleTestTimer) FireIfDue(now time.Time) {
	t.mu.Lock()
	if t.stopped || t.fired || now.Before(t.deadline) {
		t.mu.Unlock()
		return
	}
	t.fired = true
	callback := t.callback
	t.mu.Unlock()
	callback()
}

func (t *idleTestTimer) ForceFire() {
	t.mu.Lock()
	if t.fired {
		t.mu.Unlock()
		return
	}
	t.fired = true
	callback := t.callback
	t.mu.Unlock()
	callback()
}

func requireNoIdleCallback(t *testing.T, idle <-chan struct{}) {
	t.Helper()
	select {
	case <-idle:
		require.FailNow(t, "unexpected idle callback")
	default:
	}
}
