package federationcoord

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type testProjectLocker struct {
	gate sync.RWMutex
}

func (l *testProjectLocker) AcquireFederationProjectSharedLock(
	context.Context,
	int64,
) (func(), error) {
	l.gate.RLock()
	return l.gate.RUnlock, nil
}

func (l *testProjectLocker) AcquireFederationProjectExclusiveLock(
	context.Context,
	int64,
) (func(), error) {
	l.gate.Lock()
	return l.gate.Unlock, nil
}

func TestBackendProjectLockCoordinatesDistinctProcessGates(t *testing.T) {
	locker := &testProjectLocker{}
	releaseSync, err := BeginSync(t.Context(), "process-a", locker, 42)
	require.NoError(t, err)
	t.Cleanup(releaseSync)

	rebindAcquired := make(chan func(), 1)
	go func() {
		release, acquireErr := BeginRebind(t.Context(), "process-b", locker, 42)
		if acquireErr != nil {
			rebindAcquired <- nil
			return
		}
		rebindAcquired <- release
	}()

	select {
	case <-rebindAcquired:
		t.Fatal("backend rebind lock acquired while another process gate held transport")
	case <-time.After(100 * time.Millisecond):
	}
	releaseSync()

	select {
	case releaseRebind := <-rebindAcquired:
		require.NotNil(t, releaseRebind)
		releaseRebind()
	case <-time.After(time.Second):
		t.Fatal("backend rebind lock did not acquire after transport release")
	}
}
