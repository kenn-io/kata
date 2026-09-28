package tui

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestLocalProfileReconnectAcrossRuntimeEndpoints(t *testing.T) {
	for _, test := range []struct {
		network  string
		sseFirst bool
	}{{"unix", false}, {"tcp", false}, {"unix", true}, {"tcp", true}} {
		t.Run(fmt.Sprintf("%s/sse_first=%v", test.network, test.sseFirst), func(t *testing.T) {
			network := test.network
			if network == "unix" && runtime.GOOS == "windows" {
				t.Skip("unix")
			}
			personal, work := t.TempDir(), t.TempDir()
			t.Setenv("KATA_HOME", personal)
			t.Setenv("KATA_SERVER", "")
			t.Setenv("KATA_AUTH_TOKEN", "personal-token")
			store, err := sqlitestore.Open(t.Context(), filepath.Join(work, "kata.db"))
			require.NoError(t, err)
			uid := store.InstanceUID()
			require.NoError(t, store.Close())
			require.NoError(t, os.WriteFile(filepath.Join(work, "config.toml"), []byte("[auth]\ntoken=\"work-token\"\n"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(personal, "config.toml"), []byte(fmt.Sprintf("[[daemon]]\nname=\"work\"\nlocal=true\nhome=%q\ninstance_uid=%q\n", work, uid)), 0600))
			profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: work, InstanceUID: uid})
			require.NoError(t, err)
			ns, err := daemon.NewNamespaceForHome(work, profile.StorageID)
			require.NoError(t, err)
			require.NoError(t, ns.EnsureDirs())
			sockets, err := os.MkdirTemp("", "review-socket-")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(sockets) })
			var mutations atomic.Int32
			start := func(label string) *http.Server {
				address := "127.0.0.1:0"
				if network == "unix" {
					address = filepath.Join(sockets, label+".sock")
				}
				listener, err := net.Listen(network, address)
				require.NoError(t, err)
				srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						mutations.Add(1)
					}
					if r.URL.Path == "/api/v1/ping" {
						_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
						return
					}
					if r.Header.Get("Authorization") != "Bearer work-token" {
						http.Error(w, "wrong token", http.StatusUnauthorized)
						return
					}
					if r.URL.Path == "/api/v1/events" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n")
						return
					}
					_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
				})}
				go func() { _ = srv.Serve(listener) }()
				t.Cleanup(func() { _ = srv.Close() })
				_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{Service: "kata", PID: os.Getpid(), Network: network, Address: listener.Addr().String()})
				require.NoError(t, err)
				return srv
			}
			old := start("old")
			conn, err := connectDaemonTarget(t.Context(), daemonTarget{Name: "work", Local: true, Home: work, skipInitialScope: true})
			require.NoError(t, err)
			require.NoError(t, old.Close())
			next := start("new")
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if !test.sseFirst {
				_, err = conn.api.GetInstance(ctx)
				require.NoError(t, err, "API must rediscover the same local profile after endpoint replacement")
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, conn.endpoint+"/api/v1/events", nil)
			require.NoError(t, err)
			response, err := conn.sseHC.Do(req)
			require.NoError(t, err, "SSE must use the refreshed profile socket after API reconnect")
			if response != nil {
				_ = response.Body.Close()
			}
			_, err = conn.api.GetInstance(ctx)
			require.NoError(t, err, "API must share the endpoint selected by SSE reconnect")
			require.Zero(t, conn.sseHC.Timeout, "SSE must retain its streaming timeout policy")
			require.NoError(t, next.Close())
			start("latest")
			mutation, err := http.NewRequestWithContext(ctx, http.MethodPost, conn.endpoint+"/api/v1/projects", strings.NewReader(`{"name":"spoke-project"}`))
			require.NoError(t, err)
			_, err = conn.api.httpClient().Do(mutation)
			require.Error(t, err, "a non-idempotent mutation must not be replayed after transport failure")
			require.Zero(t, mutations.Load())
			_, err = conn.api.GetInstance(ctx)
			require.NoError(t, err, "later reads use the profile refreshed by a failed mutation")
		})
	}
}

func TestActiveLegacyLocalRetainsCatalogCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("active_daemon=\"work\"\n[auth]\ntoken=\"bootstrap-token\"\n[[daemon]]\nname=\"work\"\nlocal=true\ntoken=\"identity-token\"\n"), 0600))
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
			return
		}
		if r.Header.Get("Authorization") != "Bearer identity-token" {
			http.Error(w, "wrong token", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"instance_uid":"01HZZZZZZZZZZZZZZZZZZZZZ01"}`)
	}))
	defer s.Close()
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{Service: "kata", PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(s.URL, "http://")})
	require.NoError(t, err)
	conn, err := bootDaemonConnection(t.Context(), Options{ProjectName: "spoke-project"})
	require.NoError(t, err)
	_, err = conn.api.GetInstance(t.Context())
	require.NoError(t, err, "TUI active local entry must retain catalog identity-token")
}

func TestActiveLegacyLocalReconnectRetainsCatalogCredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix transport")
	}
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_WORK_TOKEN", "identity-token")
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("active_daemon=\"work\"\n[auth]\ntoken=\"bootstrap-token\"\n[[daemon]]\nname=\"work\"\nlocal=true\ntoken_env=\"KATA_WORK_TOKEN\"\n"), 0600))
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	socketDir, err := os.MkdirTemp("", "legacy-daemon-socket-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	start := func(label string) *http.Server {
		socket := filepath.Join(socketDir, label+".sock")
		listener, listenErr := net.Listen("unix", socket)
		require.NoError(t, listenErr)
		srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/ping" {
				_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
				return
			}
			if r.Header.Get("Authorization") != "Bearer identity-token" {
				http.Error(w, "wrong token", http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/api/v1/events" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n")
				return
			}
			_, _ = fmt.Fprint(w, `{"instance_uid":"01HZZZZZZZZZZZZZZZZZZZZZ01"}`)
		})}
		go func() { _ = srv.Serve(listener) }()
		t.Cleanup(func() { _ = srv.Close() })
		_, writeErr := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
			Service: "kata", PID: os.Getpid(), Network: "unix", Address: socket,
		})
		require.NoError(t, writeErr)
		return srv
	}
	assertConnection := func(conn daemonConnection) {
		_, getErr := conn.api.GetInstance(t.Context())
		require.NoError(t, getErr, "API must use the active catalog credential")
		req, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet, conn.endpoint+"/api/v1/events", nil)
		require.NoError(t, requestErr)
		response, doErr := conn.sseHC.Do(req)
		require.NoError(t, doErr, "SSE must use the active catalog credential")
		if response != nil {
			assert.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
			assert.NoError(t, response.Body.Close())
		}
	}

	old := start("old")
	conn, err := bootDaemonConnection(t.Context(), Options{ProjectName: "spoke-project"})
	require.NoError(t, err)
	assertConnection(conn)
	require.NoError(t, old.Close())
	start("new")
	_, err = conn.api.GetInstance(t.Context())
	require.NoError(t, err, "API reconnect must keep the selected catalog credential")

	reconnected, err := bootDaemonConnection(t.Context(), Options{ProjectName: "spoke-project"})
	require.NoError(t, err)
	assertConnection(reconnected)
}

func TestLocalProfileConcurrentRefreshHonorsRequestDeadline(t *testing.T) {
	personal, work := t.TempDir(), t.TempDir()
	t.Setenv("KATA_HOME", personal)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	store, err := sqlitestore.Open(t.Context(), filepath.Join(work, "kata.db"))
	require.NoError(t, err)
	uid := store.InstanceUID()
	require.NoError(t, store.Close())
	require.NoError(t, os.WriteFile(filepath.Join(personal, "config.toml"), []byte(fmt.Sprintf("[[daemon]]\nname=\"work\"\nlocal=true\nhome=%q\ninstance_uid=%q\n", work, uid)), 0600))
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: work, InstanceUID: uid})
	require.NoError(t, err)
	ns, err := daemon.NewNamespaceForHome(work, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	release, started := make(chan struct{}), make(chan struct{}, 1)
	start := func(block bool) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/ping" {
				_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
				return
			}
			if block && r.URL.Path == "/api/v1/instance" {
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
		}))
		t.Cleanup(s.Close)
		_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{Service: "kata", PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(s.URL, "http://")})
		require.NoError(t, err)
		return s
	}
	old := start(false)
	conn, err := connectDaemonTarget(t.Context(), daemonTarget{Name: "work", Local: true, Home: work, skipInitialScope: true})
	require.NoError(t, err)
	old.Close()
	start(true)
	defer close(release)
	sseCtx, cancelSSE := context.WithCancel(t.Context())
	defer cancelSSE()
	go func() {
		req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, conn.endpoint+"/api/v1/events", nil)
		resp, _ := conn.sseHC.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh never started")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := conn.api.GetInstance(ctx); done <- err }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(300 * time.Millisecond):
		t.Fatal("API request remained blocked behind SSE refresh after its own context deadline")
	}
}
