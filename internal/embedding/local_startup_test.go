package embedding

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedlocal"
)

func TestResolveLocalDoesNotDownloadMissingAssets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".kenn", "embeddings"), 0o700))
	endpointCalls := 0
	installCalls := 0
	got, err := ResolveLocal(t.Context(), func(context.Context, embedlocal.Options) (string, error) {
		endpointCalls++
		return "", embedlocal.ErrNotFound
	}, func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		installCalls++
		require.Nil(t, opts.Confirm)
		return embedlocal.Install(ctx, opts)
	}, nil)
	require.Empty(t, got)
	require.ErrorContains(t, err, "provide Confirm")
	require.Equal(t, 1, endpointCalls)
	require.Equal(t, 1, installCalls)
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
