package tui

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
)

// These are TUI boundary backends: real socket transports and authentication,
// with distinguishable API and stream responses rather than a mocked client.
func tuiUnixBackend(t *testing.T, name, token string) (clientpkg.ResolvedDaemon, *http.Server, *atomic.Int64) {
	t.Helper()
	dir, err := os.MkdirTemp("", "tui-unix-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	calls := new(atomic.Int64)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v1/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: heartbeat\ndata: {\"target\":%q}\n\n", name)
			return
		}
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, name)
	})}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return clientpkg.ResolvedDaemon{Source: clientpkg.DaemonSourceServerEnv, BaseURL: clientpkg.UnixBase, Address: "unix://" + socket, UnixSocket: socket, Network: "unix", Scheme: "http", Token: token}, srv, calls
}

func TestConfiguredUnixTUIStaysPinnedAfterDisconnect(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	a, _, aCalls := tuiUnixBackend(t, "default-a", "token-a")
	b, serverB, _ := tuiUnixBackend(t, "selected-b", "token-b")
	oldEnsure, oldRefresh := ensureResolvedForTUI, refreshLocalHTTPClientForTUI
	oldLocal := ensureLocalRunningTargetForTUI
	t.Cleanup(func() {
		ensureResolvedForTUI, refreshLocalHTTPClientForTUI = oldEnsure, oldRefresh
		ensureLocalRunningTargetForTUI = oldLocal
	})
	ensureResolvedForTUI = func(context.Context, string) (clientpkg.ResolvedDaemon, error) { return b, nil }
	refreshes := 0
	refreshLocalHTTPClientForTUI = func(ctx context.Context) (*http.Client, error) {
		refreshes++
		return clientpkg.NewHTTPClientForResolved(ctx, a, clientpkg.Opts{})
	}
	ensureLocalRunningTargetForTUI = func(context.Context) (clientpkg.RunningDaemon, error) {
		refreshes++
		return a.Running(), nil
	}
	conn, err := connectImplicitDaemonTarget(t.Context(), "", true)
	require.NoError(t, err)
	instance, err := conn.api.GetInstance(t.Context())
	require.NoError(t, err)
	require.Equal(t, "selected-b", instance.InstanceUID)
	for range 2 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, conn.endpoint+"/api/v1/events", nil)
		require.NoError(t, err)
		resp, err := conn.sseHC.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Contains(t, string(body), "selected-b")
	}
	require.NoError(t, serverB.Close())
	_, err = conn.api.GetInstance(t.Context())
	require.Error(t, err, "a stopped explicit socket must not rediscover the current home")
	_, err = conn.api.GetInstance(t.Context())
	require.Error(t, err, "later requests must remain pinned too")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, conn.endpoint+"/api/v1/events", nil)
	require.NoError(t, err)
	resp, err := conn.sseHC.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.Zero(t, refreshes)
	assert.Zero(t, aCalls.Load())
	assert.Equal(t, b.Address, daemonEndpoint(conn.target))
	assert.Equal(t, b.Address, daemonTargetDisplay(conn.target))
	assert.False(t, daemonTargetsMatch(conn.target, daemonTarget{URL: a.Address, resolved: a}))
}

func TestConfiguredUnixTUIUsesPathFreeBoot(t *testing.T) {
	for _, source := range []clientpkg.DaemonSource{clientpkg.DaemonSourceServerEnv, clientpkg.DaemonSourceLocalConfig} {
		t.Run(source.String(), func(t *testing.T) {
			resolved, _, _ := tuiUnixBackend(t, "selected", "selected-token")
			resolved.Source = source
			oldEnsure := ensureResolvedForTUI
			oldLocal, oldPathFree := bootResolveScopeForTUI, bootResolveScopePathFreeForTUI
			t.Cleanup(func() {
				ensureResolvedForTUI = oldEnsure
				bootResolveScopeForTUI, bootResolveScopePathFreeForTUI = oldLocal, oldPathFree
			})
			ensureResolvedForTUI = func(context.Context, string) (clientpkg.ResolvedDaemon, error) { return resolved, nil }
			bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
				return bootInit{}, fmt.Errorf("unsafe filesystem boot")
			}
			bootResolveScopePathFreeForTUI = func(ctx context.Context, c *Client, _ string) (bootInit, error) {
				instance, err := c.GetInstance(ctx)
				if err == nil {
					assert.Equal(t, "selected", instance.InstanceUID)
				}
				return bootInit{view: viewProjects}, err
			}
			conn, err := connectImplicitDaemonTarget(t.Context(), "", false)
			require.NoError(t, err)
			assert.Equal(t, viewProjects, conn.init.view)
		})
	}
}

func TestClientNilRefreshDisablesCurrentHomeRecovery(t *testing.T) {
	resolved, srv, _ := tuiUnixBackend(t, "selected", "token")
	hc, err := clientpkg.NewHTTPClientForResolved(t.Context(), resolved, clientpkg.Opts{})
	require.NoError(t, err)
	c := NewClient(resolved.BaseURL, hc)
	c.setLocalHTTPClientRefresh(nil)
	old := refreshLocalHTTPClientForTUI
	t.Cleanup(func() { refreshLocalHTTPClientForTUI = old })
	calls := 0
	refreshLocalHTTPClientForTUI = func(context.Context) (*http.Client, error) { calls++; return hc, nil }
	require.NoError(t, srv.Close())
	_, err = c.GetInstance(t.Context())
	require.Error(t, err)
	assert.Zero(t, calls, "nil must disable recovery, not restore the current-home default")
}
