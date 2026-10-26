package daemonruntime_test

import (
	"os"
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

// Cutover replaces the configured name with a regular file. An owner reached
// through outer -> inner -> target must still own inner once inner is replaced.
func TestStoreOwnerLockCoversIntermediateSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	inner := filepath.Join(dir, "inner.db")
	outer := filepath.Join(dir, "outer.db")
	require.NoError(t, os.WriteFile(target, nil, 0o600))
	if err := os.Symlink(target, inner); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	require.NoError(t, os.Symlink(inner, outer))
	release, err := daemonruntime.AcquireStoreOwnerLock(t.Context(), daemonruntime.StoreOwnerLockPaths(dir, outer))
	require.NoError(t, err)
	defer release()

	require.NoError(t, os.Remove(inner))
	require.NoError(t, os.WriteFile(inner, nil, 0o600))
	require.True(t, daemonruntime.StoreOwnerLocked(daemonruntime.StoreOwnerLockPaths(dir, inner)))
}
