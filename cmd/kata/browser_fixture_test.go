//go:build kata_browser_fixture

package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
	require.Zero(t, calls)
}
