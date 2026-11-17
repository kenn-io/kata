package embedding

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedlocal"
)

func TestLocalEndpointStartsInstalledServer(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	endpoint := func(_ context.Context, opts embedlocal.Options) (string, error) {
		calls++
		require.True(t, opts.SkipOllama)
		require.Equal(t, dir, opts.DataDir)
		if calls == 1 {
			require.Empty(t, opts.Executable)
			return "", embedlocal.ErrNotFound
		}
		require.Equal(t, "/installed/kennembed", opts.Executable)
		return "http://127.0.0.1:1234/v1", nil
	}
	install := func(_ context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		require.Equal(t, dir, opts.DataDir)
		require.Nil(t, opts.Confirm)
		return embedlocal.Installation{DataDir: dir, Executable: "/installed/kennembed"}, nil
	}
	got, err := localEndpoint(t.Context(), dir, endpoint, install)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:1234/v1", got)
	require.Equal(t, 2, calls)
}

func TestLocalEndpointDoesNotDownloadMissingAssets(t *testing.T) {
	dir := t.TempDir()
	got, err := localEndpoint(t.Context(), dir, func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }, embedlocal.Install)
	require.Empty(t, got)
	require.Error(t, err)
	require.False(t, errors.Is(err, embedlocal.ErrNotFound))
}

func TestLocalEndpointReusesRunningServer(t *testing.T) {
	endpoint := func(_ context.Context, opts embedlocal.Options) (string, error) {
		require.True(t, opts.SkipOllama)
		require.Empty(t, opts.Executable)
		return "http://127.0.0.1:1234/v1", nil
	}
	install := func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
		t.Fatal("running server must not install")
		return embedlocal.Installation{}, nil
	}
	got, err := localEndpoint(t.Context(), t.TempDir(), endpoint, install)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:1234/v1", got)
}
