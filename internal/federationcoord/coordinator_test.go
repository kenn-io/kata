package federationcoord

import (
	"context"
	"errors"
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
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: absence window; the process-a sync gate holds transport, so the rebind lock must not be granted
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

func TestTrySyncBusyAndShared(t *testing.T) {
	release, err := BeginRebind(t.Context(), t.Name(), nil, 1)
	require.NoError(t, err)
	start := time.Now()
	_, err = TryBeginSync(t.Context(), t.Name(), nil, 1)
	require.ErrorIs(t, err, ErrBusy)
	require.Less(t, time.Since(start), time.Second)
	release()
	a, err := TryBeginSync(t.Context(), t.Name(), nil, 1)
	require.NoError(t, err)
	defer a()
	b, err := TryBeginSync(t.Context(), t.Name(), nil, 1)
	require.NoError(t, err)
	b()
	b()
}

type tryProjectLocker struct{}

func (tryProjectLocker) AcquireFederationProjectSharedLock(context.Context, int64) (func(), error) {
	return nil, errors.New("blocking path must not be used")
}
func (tryProjectLocker) TryAcquireFederationProjectSharedLock(context.Context, int64) (func(), bool, error) {
	return nil, false, nil
}
func TestTrySyncBackendBusy(t *testing.T) {
	_, err := TryBeginSync(t.Context(), t.Name(), tryProjectLocker{}, 1)
	require.ErrorIs(t, err, ErrBusy)
	release, err := TryBeginSync(t.Context(), t.Name(), nil, 1)
	require.NoError(t, err)
	release()
}
