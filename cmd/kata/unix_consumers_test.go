package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/testenv"
)

// Expose the real authenticated daemon and SQLite store through an independent
// Unix endpoint, without publishing a runtime record for that endpoint.
func unixConsumerEndpoint(t *testing.T, target string, observers ...func(*http.Request)) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "kata-cli-uds-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "api.sock")
	listener, err := new(net.ListenConfig).Listen(t.Context(), "unix", path)
	require.NoError(t, err)
	upstream, err := url.Parse(target)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, observe := range observers {
			observe(r)
		}
		proxy.ServeHTTP(w, r)
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	stop := func() { _ = server.Close() }
	t.Cleanup(stop)
	return path, stop
}

func TestUnixConsumerHealthAndDiagnosisPreserveEndpoint(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t, testenv.WithAuthToken("endpoint-test-token"))
	path, _ := unixConsumerEndpoint(t, env.URL)
	t.Setenv("KATA_SERVER", "unix://"+path)
	for _, args := range [][]string{{"health", "--json"}, {"daemon", "diagnose", "--json"}, {"daemon", "locate", "--json"}} {
		out, _, err := executeRootCapture(t, t.Context(), args...)
		require.NoError(t, err)
		assert.Contains(t, out, "unix://"+path)
		assert.NotContains(t, out, "kata.invalid")
	}
}

func TestUnixConsumerTransportErrorPreservesEndpoint(t *testing.T) {
	resetFlags(t)
	path := filepath.Join(t.TempDir(), "missing.sock")
	resolved := clientpkg.ResolvedDaemon{Source: clientpkg.DaemonSourceServerEnv, BaseURL: clientpkg.UnixBase, Address: "unix://" + path, Network: "unix", UnixSocket: path}
	hc, err := httpClientForResolved(t.Context(), resolved)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, clientpkg.UnixBase+"/api/v1/projects", nil)
	require.NoError(t, err)
	_, err = hc.Do(req)
	var transportErr *daemonTransportError
	require.True(t, errors.As(err, &transportErr))
	assert.Equal(t, "unix://"+path, transportErr.selectedOrigin)
}

func TestUnixConsumerFederationKeepsSelectedTransportAndCredentialPolicy(t *testing.T) {
	for _, selection := range []string{"environment", "workspace"} {
		testUnixConsumerFederationTarget(t, selection)
	}
}

func testUnixConsumerFederationTarget(t *testing.T, selection string) {
	t.Helper()
	t.Run(selection, func(t *testing.T) {
		resetFlags(t)
		spoke := testenv.New(t, testenv.WithAuthToken("spoke-token"))
		_, err := spoke.DB.CreateProject(t.Context(), "only-spoke")
		require.NoError(t, err)
		hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
		_, err = hub.DB.CreateProject(t.Context(), "only-hub")
		require.NoError(t, err)
		authorization := make(chan []string, 4)
		path, stop := unixConsumerEndpoint(t, spoke.URL, func(r *http.Request) {
			if r.URL.Path == "/api/v1/projects" {
				authorization <- r.Header.Values("Authorization")
			}
		})
		anonymous := selection == "environment" || selection == "workspace"
		requireAuthorization := func() {
			t.Helper()
			select {
			case got := <-authorization:
				if anonymous {
					require.Empty(t, got, "URL selection must not send any ambient or catalog Authorization header")
				} else {
					require.Equal(t, []string{"Bearer spoke-token"}, got, "catalog selection must send only the selected spoke token")
				}
			default:
				t.Fatal("request did not reach the selected Unix endpoint")
			}
		}
		// Keep workspace discovery inside the test-owned directory even for
		// environment and catalog selections.
		flags.Workspace = t.TempDir()
		t.Setenv("KATA_SERVER", "")
		t.Setenv("KATA_AUTH_TOKEN", "hub-token")
		body := fmt.Sprintf("[[daemon]]\nname = \"spoke\"\nurl = %q\ntoken = \"spoke-token\"\n", "unix://"+path)
		switch selection {
		case "named":
			flags.Daemon = "spoke"
		case "active":
			body = "active_daemon = \"spoke\"\n" + body
		case "environment":
			t.Setenv("KATA_SERVER", "unix://"+path)
		case "workspace":
			require.NoError(t, os.WriteFile(filepath.Join(flags.Workspace, ".kata.toml"), []byte("version = 1\n[project]\nname = \"spoke-project\"\n"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(flags.Workspace, ".kata.local.toml"), []byte(fmt.Sprintf("version = 1\n[server]\nurl = %q\n", "unix://"+path)), 0600))
		}
		require.NoError(t, os.WriteFile(filepath.Join(hub.Home, "config.toml"), []byte(body), 0600))
		ctx, base, err := ensureSpokeDaemonContext(t.Context())
		require.NoError(t, err)
		hc, err := federationSpokeHTTPClient(ctx, base)
		require.NoError(t, err)
		mutation, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/projects", bytes.NewBufferString(`{"name":"written-spoke","actor":"operator"}`))
		require.NoError(t, err)
		mutation.Header.Set("Content-Type", "application/json")
		created, err := hc.Do(mutation)
		require.NoError(t, err)
		requireAuthorization()
		if anonymous {
			assert.Equal(t, http.StatusUnauthorized, created.StatusCode)
		} else {
			assert.Equal(t, http.StatusOK, created.StatusCode)
		}
		require.NoError(t, created.Body.Close())
		readBack := func(env *testenv.Env, token string) string {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, env.URL+"/api/v1/projects", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := env.HTTP.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			return string(body)
		}
		spokeState := readBack(spoke, "spoke-token")
		hubState := readBack(hub, "hub-token")
		if anonymous {
			assert.NotContains(t, spokeState, "written-spoke")
		} else {
			assert.Contains(t, spokeState, "written-spoke")
		}
		assert.NotContains(t, hubState, "written-spoke")
		read := func(ctx context.Context) (*http.Response, error) {
			req, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/projects", nil)
			require.NoError(t, requestErr)
			return hc.Do(req)
		}
		resp, err := read(ctx)
		require.NoError(t, err)
		requireAuthorization()
		if anonymous {
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "ambient hub credentials must not authorize the spoke")
		} else {
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			output, readErr := io.ReadAll(resp.Body)
			require.NoError(t, readErr)
			assert.Contains(t, string(output), "only-spoke")
			assert.NotContains(t, string(output), "only-hub")
		}
		require.NoError(t, resp.Body.Close())
		stop()
		_, err = read(ctx)
		require.Error(t, err, "stopped selected socket must not fall back to a local daemon")
	})
}
