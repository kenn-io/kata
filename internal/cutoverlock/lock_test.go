package cutoverlock_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cutoverlock"
	"go.kenn.io/kata/internal/jsonl"
)

func TestCutoverReadLockCoordinatesReadersAndCutover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kata.db")
	require.NoError(t, os.WriteFile(path, nil, 0600))
	release, busy, err := cutoverlock.TryReadLock(path, true)
	require.NoError(t, err)
	require.False(t, busy)
	info, err := os.Stat(path + ".import.lock")
	require.NoError(t, err)
	release2, busy, err := cutoverlock.TryReadLock(path, true)
	require.NoError(t, err)
	require.False(t, busy)
	ctx, cancel := context.WithTimeout(t.Context(), 1100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, jsonl.WithCutoverLock(ctx, path, func() error { return nil }), context.DeadlineExceeded)
	release2()
	release()
	require.NoError(t, jsonl.WithCutoverLock(t.Context(), path, func() error { return nil }))
	release, busy, err = cutoverlock.TryReadLock(path, true)
	require.NoError(t, err)
	require.False(t, busy)
	defer release()
	info2, err := os.Stat(path + ".import.lock")
	require.NoError(t, err)
	require.True(t, os.SameFile(info, info2))
}

func TestCutoverReadLockMissingAndSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "link.db")
	target := filepath.Join(dir, "target.db")
	release, busy, err := cutoverlock.TryReadLock(path, false)
	require.NoError(t, err)
	require.False(t, busy)
	release()
	_, err = os.Stat(path + ".import.lock")
	require.True(t, errors.Is(err, os.ErrNotExist))
	held := flock.New(path + ".import.lock")
	require.NoError(t, held.Lock())
	_, busy, err = cutoverlock.TryReadLock(path, false)
	require.NoError(t, err)
	require.True(t, busy)
	require.NoError(t, held.Close())
	t.Run("read-only directory", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "kata.db")
		require.NoError(t, os.WriteFile(path, nil, 0600))
		require.NoError(t, os.Chmod(dir, 0500))       //nolint:gosec // G302: exercise a read-only directory.
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) }) //nolint:gosec // G302: restore access for cleanup.
		release, busy, err := cutoverlock.TryReadLock(path, true)
		require.NoError(t, err)
		require.False(t, busy)
		release()
		_, err = os.Stat(path + ".import.lock")
		require.ErrorIs(t, err, os.ErrNotExist)
	})
	require.NoError(t, os.WriteFile(target, nil, 0600))
	if err := os.Symlink(target, path); err != nil {
		t.Skip(err)
	}
	require.NoError(t, jsonl.WithCutoverLock(t.Context(), target, func() error {
		_, busy, err := cutoverlock.TryReadLock(path, true)
		require.NoError(t, err)
		require.True(t, busy)
		return nil
	}))
}
