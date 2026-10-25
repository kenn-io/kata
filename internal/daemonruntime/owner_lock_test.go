package daemonruntime_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
)

func TestStoreOwnerLockedReportsHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kata.db.owner.lock")
	require.False(t, daemonruntime.StoreOwnerLocked(path), "a missing lock file has no owner")
	release, err := daemonruntime.AcquireStoreOwnerLock(t.Context(), path)
	require.NoError(t, err)
	require.True(t, daemonruntime.StoreOwnerLocked(path))
	release()
	require.False(t, daemonruntime.StoreOwnerLocked(path), "the probe must not keep the lock")
}
