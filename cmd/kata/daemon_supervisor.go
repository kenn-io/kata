package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// daemonWorkerGroup owns every background goroutine that can touch daemon
// dependencies. Workers are registered during startup and joined after root
// cancellation before those dependencies are closed.
type daemonWorkerGroup struct {
	mu       sync.Mutex
	wg       sync.WaitGroup
	stopping bool
}

type daemonShutdownTrigger struct {
	mu      sync.RWMutex
	trigger func()
}

func newDaemonShutdownTrigger(trigger func()) *daemonShutdownTrigger {
	return &daemonShutdownTrigger{trigger: trigger}
}

func (t *daemonShutdownTrigger) Set(trigger func()) {
	t.mu.Lock()
	t.trigger = trigger
	t.mu.Unlock()
}

func (t *daemonShutdownTrigger) Call() {
	t.mu.RLock()
	trigger := t.trigger
	t.mu.RUnlock()
	if trigger != nil {
		trigger()
	}
}

func newDaemonWorkerGroup() *daemonWorkerGroup {
	return &daemonWorkerGroup{}
}

func (g *daemonWorkerGroup) Go(worker func()) bool {
	if g == nil || worker == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return false
	}
	g.wg.Go(func() {
		worker()
	})
	return true
}

func (g *daemonWorkerGroup) Wait(ctx context.Context) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	g.stopping = true
	g.mu.Unlock()
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

type daemonHookShutdown interface {
	BeginProducerDrain()
	Shutdown(context.Context) error
}

type daemonPlatformCleanup func(context.Context) bool

type daemonShutdownResult struct {
	workerErr   error
	httpErr     error
	hookErr     error
	platformErr error
}

func (r daemonShutdownResult) Err() error {
	return errors.Join(r.workerErr, r.httpErr, r.hookErr, r.platformErr)
}

func (r daemonShutdownResult) SafeToCloseDependencies() bool {
	return r.workerErr == nil && r.httpErr == nil && r.hookErr == nil && r.platformErr == nil
}

type daemonShutdownCoordinator struct {
	rootCancel    context.CancelFunc
	stopAdmission func()
	hooks         daemonHookShutdown
	timeout       time.Duration
	stateMu       sync.Mutex
	stopping      bool
	triggerOnce   sync.Once
	triggered     chan struct{}
	httpDoneOnce  sync.Once
	httpDone      chan bool
	deadline      time.Time
	done          chan struct{}
	result        daemonShutdownResult
}

var errDaemonStoppingBeforeReady = errors.New("kata daemon: shutdown started before readiness")

func startDaemonShutdownCoordinator(
	ctx context.Context,
	rootCancel context.CancelFunc,
	workers *daemonWorkerGroup,
	hooks daemonHookShutdown,
	stopAdmission func(),
	timeout time.Duration,
	platformCleanups ...daemonPlatformCleanup,
) *daemonShutdownCoordinator {
	coordinator := &daemonShutdownCoordinator{
		rootCancel:    rootCancel,
		stopAdmission: stopAdmission,
		hooks:         hooks,
		timeout:       timeout,
		triggered:     make(chan struct{}),
		httpDone:      make(chan bool, 1),
		done:          make(chan struct{}),
	}
	go func() {
		select {
		case <-ctx.Done():
			coordinator.Trigger()
		case <-coordinator.triggered:
		}
	}()
	go func() {
		defer close(coordinator.done)
		<-coordinator.triggered
		// Root cancellation starts shutdown, so retain its values without
		// inheriting the cancellation that the bounded drain must outlive.
		shutdownCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), coordinator.deadline)
		defer cancel()

		workerDone := make(chan bool, 1)
		go func() { workerDone <- workers.Wait(shutdownCtx) }()
		platformDone := make(chan bool, len(platformCleanups))
		for _, cleanup := range platformCleanups {
			go func(cleanup daemonPlatformCleanup) {
				platformDone <- cleanup == nil || cleanup(shutdownCtx)
			}(cleanup)
		}

		if joined := <-workerDone; !joined {
			coordinator.result.workerErr = fmt.Errorf(
				"kata daemon: background workers did not stop within %s: %w",
				coordinator.timeout,
				context.DeadlineExceeded,
			)
		}
		select {
		case joined := <-coordinator.httpDone:
			if !joined {
				<-shutdownCtx.Done()
				coordinator.result.httpErr = fmt.Errorf(
					"kata daemon: HTTP handlers did not stop within the shutdown deadline: %w",
					context.DeadlineExceeded,
				)
			}
		case <-shutdownCtx.Done():
			coordinator.result.httpErr = fmt.Errorf(
				"kata daemon: HTTP handlers did not report shutdown completion: %w",
				shutdownCtx.Err(),
			)
		}

		producersJoined := coordinator.result.workerErr == nil && coordinator.result.httpErr == nil
		if producersJoined && coordinator.hooks != nil {
			if hookErr := coordinator.hooks.Shutdown(shutdownCtx); hookErr != nil {
				coordinator.result.hookErr = fmt.Errorf("kata daemon: hooks did not stop: %w", hookErr)
			}
		}
		for range platformCleanups {
			if joined := <-platformDone; !joined {
				cause := shutdownCtx.Err()
				if cause == nil {
					cause = errors.New("cleanup reported incomplete")
				}
				coordinator.result.platformErr = errors.Join(
					coordinator.result.platformErr,
					fmt.Errorf("kata daemon: platform cleanup did not stop: %w", cause),
				)
			}
		}
	}()
	return coordinator
}

// HTTPHandlersDone reports whether every request handler joined. Hook shutdown
// waits for this signal and for background workers because both can enqueue
// post-commit hook jobs.
func (c *daemonShutdownCoordinator) HTTPHandlersDone(joined bool) {
	if c == nil {
		return
	}
	c.httpDoneOnce.Do(func() { c.httpDone <- joined })
}

func (c *daemonShutdownCoordinator) Trigger() {
	if c == nil {
		return
	}
	c.triggerOnce.Do(func() {
		c.stateMu.Lock()
		c.stopping = true
		c.deadline = time.Now().Add(c.timeout)
		c.stateMu.Unlock()
		// Producers may commit work after cancellation. Keep their hook handoff
		// path open before sealing top-level idle admission.
		if c.hooks != nil {
			c.hooks.BeginProducerDrain()
		}
		if c.stopAdmission != nil {
			c.stopAdmission()
		}
		if c.rootCancel != nil {
			c.rootCancel()
		}
		close(c.triggered)
	})
}

// PublishReady serializes readiness publication with shutdown. If shutdown
// wins the lifecycle gate, publish is not called; if publication wins, a later
// shutdown observes a daemon that was legitimately ready first. The callback
// runs under the lifecycle lock and must remain bounded and non-reentrant.
func (c *daemonShutdownCoordinator) PublishReady(ctx context.Context, publish func() error) error {
	if c == nil || publish == nil {
		return nil
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.stopping || ctx.Err() != nil {
		return errDaemonStoppingBeforeReady
	}
	return publish()
}

func (c *daemonShutdownCoordinator) Wait() daemonShutdownResult {
	if c == nil {
		return daemonShutdownResult{}
	}
	<-c.done
	return c.result
}
