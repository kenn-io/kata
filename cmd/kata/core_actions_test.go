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
	for _, tc := range []struct {
		name  string
		tty   bool
		hook  string
		flags []string
		want  bool
	}{
		{name: "terminal", tty: true, want: true},
		{name: "pipe"},
		{name: "agent", tty: true, flags: []string{"--agent"}},
		{name: "agent format", tty: true, flags: []string{"--format", "agent"}},
		{name: "hook", tty: true, hook: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			stubIsTTY(t, tc.tty)
			t.Setenv(hooks.HookVersionEnv, tc.hook)
			t.Cleanup(func() { cliUseTarget.Store(nil) })
			args := append([]string{"create", "Example task", "--workspace", dir}, tc.flags...)
			_, err = runCmdOutput(t, env, args...)
			require.NoError(t, err)
			args = append([]string{"close", ref, "--workspace", dir, "--done", "--message", "Completed the requested work and checked the result.", "--commit", "abc1234"}, tc.flags...)
			_, err = runCmdOutput(t, env, args...)
			require.NoError(t, err)
			mu.Lock()
			defer mu.Unlock()
			if tc.want {
				assert.Equal(t, []string{"cli", "cli"}, surfaces)
			} else {
				assert.Equal(t, []string{"", ""}, surfaces)
			}
		})
	}
}
