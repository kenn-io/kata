package jsonl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cutoverlock"
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
	require.ErrorIs(t, WithCutoverLock(ctx, path, func() error { return nil }), context.DeadlineExceeded)
	release2()
	release()
	require.NoError(t, WithCutoverLock(t.Context(), path, func() error { return nil }))
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
	require.NoError(t, os.WriteFile(target, nil, 0600))
	if err := os.Symlink(target, path); err != nil {
		t.Skip(err)
	}
	require.NoError(t, WithCutoverLock(t.Context(), target, func() error {
		_, busy, err := cutoverlock.TryReadLock(path, true)
		require.NoError(t, err)
		require.True(t, busy)
		return nil
	}))
	_, err = os.Stat(path + ".import.lock")
	require.NoError(t, err)
	_, err = os.Stat(target + ".import.lock")
	require.NoError(t, err)
}
