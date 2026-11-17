package embedding

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedlocal"
)

func TestResolveLocalDoesNotDownloadWithoutConfirmation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	endpointCalls := 0
	got, err := ResolveLocal(t.Context(), func(context.Context, embedlocal.Options) (string, error) {
		endpointCalls++
		return "", embedlocal.ErrNotFound
	}, embedlocal.Install, nil)
	require.Empty(t, got)
	require.Error(t, err)
	require.False(t, errors.Is(err, embedlocal.ErrNotFound))
	require.ErrorContains(t, err, "provide Confirm")
	require.Equal(t, 1, endpointCalls)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		info, err := entry.Info()
		require.NoError(t, err)
		require.False(t, info.IsDir())
		require.Zero(t, info.Size())
	}
}

func TestInstallLocalCallbacks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	expectedDir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	var downloaded, total int64
	install := func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		require.Equal(t, expectedDir, opts.DataDir)
		approved, err := opts.Confirm(ctx, 200)
		require.NoError(t, err)
		require.True(t, approved)
		opts.Progress(50, 200)
		return embedlocal.Installation{Executable: "/example/kennembed"}, nil
	}
	installed, err := InstallLocal(t.Context(), install, func(_ context.Context, size int64) (bool, error) {
		return size == 200, nil
	}, func(done, size int64) { downloaded, total = done, size })
	require.NoError(t, err)
	require.Equal(t, "/example/kennembed", installed.Executable)
	require.EqualValues(t, 50, downloaded)
	require.EqualValues(t, 200, total)
}
