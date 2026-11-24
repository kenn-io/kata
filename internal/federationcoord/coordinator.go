// Package federationcoord serializes endpoint changes with project-scoped
// federation transport work inside one daemon process.
package federationcoord

import (
	"context"
	"errors"
	"strconv"
	"sync"
)

var projectGates sync.Map
var heldRequests sync.Map

type heldSet struct {
	mu       sync.Mutex
	requests map[chan struct{}]context.CancelFunc
}

// BeginHeld registers a request while its caller holds the sync gate.
func BeginHeld(key string, cancel context.CancelFunc) func() {
	value, _ := heldRequests.LoadOrStore(key, &heldSet{requests: make(map[chan struct{}]context.CancelFunc)})
	held := value.(*heldSet)
	done := make(chan struct{})
	held.mu.Lock()
	held.requests[done] = cancel
	held.mu.Unlock()
	return func() {
		held.mu.Lock()
		delete(held.requests, done)
		close(done)
		held.mu.Unlock()
	}
}

// Key identifies one project in one durable daemon instance.
func Key(instanceUID string, projectID int64) string {
	return instanceUID + "\x00" + strconv.FormatInt(projectID, 10)
}

// BeginSync admits one shared transport operation. The returned function must
// be called when all remote responses and local commits are complete.
func BeginSync(
	ctx context.Context,
	key string,
	backend any,
	projectID int64,
) (func(), error) {
	gate := gateFor(key)
	gate.RLock()
	backendRelease := func() {}
	if locker, ok := backend.(interface {
		AcquireFederationProjectSharedLock(context.Context, int64) (func(), error)
	}); ok {
		var err error
		backendRelease, err = locker.AcquireFederationProjectSharedLock(ctx, projectID)
		if err != nil {
			gate.RUnlock()
			return nil, err
		}
	}
	return coordinatedRelease(backendRelease, gate.RUnlock), nil
}

// ErrBusy asks an interactive caller to retry after an endpoint change finishes.
var ErrBusy = errors.New("federation endpoint is busy; retry shortly")

// TryBeginSync admits shared transport without waiting behind a local rebind.
func TryBeginSync(ctx context.Context, key string, backend any, projectID int64) (func(), error) {
	gate := gateFor(key)
	if !gate.TryRLock() {
		return nil, ErrBusy
	}
	backendRelease := func() {}
	if locker, ok := backend.(interface {
		TryAcquireFederationProjectSharedLock(context.Context, int64) (func(), bool, error)
	}); ok {
		release, acquired, err := locker.TryAcquireFederationProjectSharedLock(ctx, projectID)
		if err != nil {
			gate.RUnlock()
			return nil, err
		}
		if !acquired {
			gate.RUnlock()
			return nil, ErrBusy
		}
		backendRelease = release
	}
	return coordinatedRelease(backendRelease, gate.RUnlock), nil
}

// BeginRebind drains existing transport operations and prevents new ones from
// starting until the returned function is called.
func BeginRebind(
	ctx context.Context,
	key string,
	backend any,
	projectID int64,
) (func(), error) {
	gate := gateFor(key)
	gate.Lock()
	if value, ok := heldRequests.Load(key); ok {
		held := value.(*heldSet)
		held.mu.Lock()
		pending := make([]chan struct{}, 0, len(held.requests))
		for done, cancel := range held.requests {
			cancel()
			pending = append(pending, done)
		}
		held.mu.Unlock()
		for _, done := range pending {
			select {
			case <-done:
			case <-ctx.Done():
				gate.Unlock()
				return nil, ctx.Err()
			}
		}
	}
	backendRelease := func() {}
	if locker, ok := backend.(interface {
		AcquireFederationProjectExclusiveLock(context.Context, int64) (func(), error)
	}); ok {
		var err error
		backendRelease, err = locker.AcquireFederationProjectExclusiveLock(ctx, projectID)
		if err != nil {
			gate.Unlock()
			return nil, err
		}
	}
	return coordinatedRelease(backendRelease, gate.Unlock), nil
}

func coordinatedRelease(backendRelease, localRelease func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			backendRelease()
			localRelease()
		})
	}
}

func gateFor(key string) *sync.RWMutex {
	gate, _ := projectGates.LoadOrStore(key, &sync.RWMutex{})
	return gate.(*sync.RWMutex)
}
