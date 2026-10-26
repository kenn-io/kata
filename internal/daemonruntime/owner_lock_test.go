package daemonruntime_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
)

func TestStoreOwnerLockedReportsHeldLock(t *testing.T) {
	paths := []string{filepath.Join(t.TempDir(), "kata.db.owner.lock")}
	require.False(t, daemonruntime.StoreOwnerLocked(paths), "a missing lock file has no owner")
	release, err := daemonruntime.AcquireStoreOwnerLock(t.Context(), paths)
	require.NoError(t, err)
	require.True(t, daemonruntime.StoreOwnerLocked(paths))
	release()
	require.False(t, daemonruntime.StoreOwnerLocked(paths), "the probe must not keep the lock")
}
