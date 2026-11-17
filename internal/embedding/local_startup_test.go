package embedding

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedlocal"
)

func TestResolveLocalStopsAfterInstallFailure(t *testing.T) {
	installErr := errors.New("install refused")
	endpointCalls := 0
	installCalls := 0
	got, err := ResolveLocal(t.Context(), func(context.Context, embedlocal.Options) (string, error) {
		endpointCalls++
		return "", embedlocal.ErrNotFound
	}, func(_ context.Context, _ embedlocal.InstallOptions) (embedlocal.Installation, error) {
		installCalls++
		return embedlocal.Installation{}, installErr
	}, nil)
	require.Empty(t, got)
	require.ErrorIs(t, err, installErr)
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
