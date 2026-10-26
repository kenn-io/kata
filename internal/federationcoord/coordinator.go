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
	} else if _, ok := backend.(interface {
		AcquireFederationProjectSharedLock(context.Context, int64) (func(), error)
	}); ok {
		// A backend with only blocking admission cannot satisfy interactive writes.
		gate.RUnlock()
		return nil, ErrBusy
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
