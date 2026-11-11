//go:build kata_browser_fixture

package main

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedlocal"
)

func TestBrowserFixtureDoesNotDiscoverHostOllama(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	previous := http.DefaultTransport
	var calls int
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	status, err := (&localEmbeddingsSetup{}).Status(t.Context())
	require.NoError(t, err)
	require.False(t, status.OllamaAvailable)
	_, err = localEmbeddingsEndpoint(t.Context(), embedlocal.Options{})
	require.ErrorIs(t, err, embedlocal.ErrNotFound)
	require.Zero(t, calls)
}

func TestBrowserFixtureRequiresSyntheticInstaller(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("KATA_BROWSER_EMBEDDINGS_FIXTURE", filepath.Join(home, "missing"))
	var confirmed bool
	_, err := localEmbeddingsInstall(t.Context(), embedlocal.InstallOptions{
		DataDir: home,
		Confirm: func(context.Context, int64) (bool, error) { confirmed = true; return false, nil },
	})
	require.ErrorContains(t, err, "browser embedding fixture unavailable")
	require.False(t, confirmed)
}
