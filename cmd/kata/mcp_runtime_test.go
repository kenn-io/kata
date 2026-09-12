package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestMCPRuntimeDirectorySelectsAttachedDaemon(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	directory := t.TempDir()
	record := kitdaemon.NewRuntimeRecord("kata", "test", kitdaemon.Endpoint{Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
	_, err := (kitdaemon.RuntimeStore{Dir: directory}).Write(record)
	require.NoError(t, err)
	resolved, err := resolveMCPRuntime(t.Context(), directory)
	require.NoError(t, err)
	hc, err := httpClientForResolved(t.Context(), resolved)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, resolved.BaseURL+"/selected", nil)
	require.NoError(t, err)
	response, err := hc.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	assert.Equal(t, http.StatusAccepted, response.StatusCode)
}
