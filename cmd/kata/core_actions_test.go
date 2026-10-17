package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/hooks"
)

func TestCoreActionCLIHeaders(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "Close target")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
	var mu sync.Mutex
	var surfaces []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/issues") || strings.HasSuffix(r.URL.Path, "/actions/close")) {
			mu.Lock()
			surfaces = append(surfaces, r.Header.Get("X-Kata-Surface"))
			mu.Unlock()
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	env.URL = server.URL
	stubIsTTY(t, true)
	t.Setenv(hooks.HookVersionEnv, "")
	t.Cleanup(func() { cliUseTarget.Store(nil) })
	_, err = runCmdOutput(t, env, "create", "Example task", "--workspace", dir)
	require.NoError(t, err)
	_, err = runCmdOutput(t, env, "close", ref, "--workspace", dir, "--done", "--message", "Completed the requested work and checked the result.", "--commit", "abc1234")
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"cli", "cli"}, surfaces)
}
