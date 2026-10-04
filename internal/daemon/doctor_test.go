package daemon_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/diagnostics"
)

type doctorSink struct{ reads atomic.Int32 }

func (*doctorSink) Enqueue(db.Event) {}
func (s *doctorSink) Diagnostics() diagnostics.Hooks {
	s.reads.Add(1)
	return diagnostics.Hooks{Available: true, Hooks: []diagnostics.Hook{}}
}

func TestDoctorEndpointRequiresOperatorAuthorityBeforeCollection(t *testing.T) {
	for _, tc := range []struct {
		name, token, bearer string
		readonly, private   bool
		principal           daemon.PrincipalKind
		want                int
	}{
		{name: "owner local", want: 200},
		{name: "static bearer", token: "tok", bearer: "tok", want: 200},
		{name: "missing bearer", token: "tok", want: 401},
		{name: "anonymous readonly", readonly: true, want: 403},
		{name: "tokenless private", private: true, want: 403},
		{name: "identity principal", principal: daemon.PrincipalDBToken, want: 403},
		{name: "browser local", principal: daemon.PrincipalWebLocal, want: 403},
		{name: "trusted proxy", principal: daemon.PrincipalTrustedProxy, want: 403},
		{name: "bootstrap", principal: daemon.PrincipalBootstrap, want: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			sink := &doctorSink{}
			ts := startTestServer(t, daemon.ServerConfig{DB: d.db, StartedAt: d.now, Hooks: sink, Auth: config.AuthConfig{Token: tc.token, AllowUnauthenticatedPrivateNetworkWrites: tc.private}, InsecureReadonly: tc.readonly})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/doctor", nil)
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			if tc.principal != "" {
				req = req.WithContext(daemon.WithPrincipal(req.Context(), daemon.Principal{Kind: tc.principal}))
			}
			rr := httptest.NewRecorder()
			ts.Config.Handler.ServeHTTP(rr, req)
			require.Equal(t, tc.want, rr.Code, rr.Body.String())
			if tc.want == 200 {
				var body struct {
					Hooks diagnostics.Hooks `json:"hooks"`
				}
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
				require.True(t, body.Hooks.Available)
				require.EqualValues(t, 1, sink.reads.Load())
			} else {
				require.Zero(t, sink.reads.Load(), "denied callers must not collect operator diagnostics")
			}
		})
	}
}

func TestAnonymousReadonlyBrowserCannotReadDoctorDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name             string
		kind             daemon.ListenerKind
		backendAuthority string
	}{
		{name: "dedicated browser listener", kind: daemon.ListenerBrowser},
		{name: "browser-marked shared listener", kind: daemon.ListenerSharedTCP, backendAuthority: "127.0.0.1:27123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			sink := &doctorSink{}
			origin := "http://127.0.0.1:27123"
			manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
				Origin: origin, InstanceID: "exampleinstance", Writable: false,
			})
			require.NoError(t, err)
			server := daemon.NewServer(daemon.ServerConfig{
				DB: d.db, StartedAt: d.now, Hooks: sink, WebSessions: manager, InsecureReadonly: true,
			})
			t.Cleanup(func() { _ = server.Close() })
			handler, err := server.HandlerFor(daemon.ListenerPolicy{
				Kind: tc.kind, Origin: origin, BackendAuthority: tc.backendAuthority,
				RequireBrowserSession: true, AllowLocalSession: true,
			})
			require.NoError(t, err)

			req := httptest.NewRequest(http.MethodGet, origin+"/api/v1/doctor", nil)
			req.Host = "127.0.0.1:27123"
			req.RemoteAddr = "127.0.0.1:54321"
			req.Header.Set("Origin", origin)
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey,
				&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 27123}))
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			require.Equal(t, http.StatusUnauthorized, rr.Code, rr.Body.String())
			require.Zero(t, sink.reads.Load(), "anonymous browser requests must not collect operator diagnostics")
		})
	}
}

func TestDoctorBrowserListenerAllowsConfiguredOperatorBearer(t *testing.T) {
	d := openTestDB(t)
	sink := &doctorSink{}
	origin := "http://127.0.0.1:27123"
	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
		Origin: origin, InstanceID: "exampleinstance", Writable: false,
	})
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{
		DB: d.db, StartedAt: d.now, Hooks: sink, WebSessions: manager,
		Auth: config.AuthConfig{Token: "operator-token"}, InsecureReadonly: true,
	})
	t.Cleanup(func() { _ = server.Close() })
	handler, err := server.HandlerFor(daemon.ListenerPolicy{
		Kind: daemon.ListenerBrowser, Origin: origin,
		RequireBrowserSession: true, AllowLocalSession: true,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, origin+"/api/v1/doctor", nil)
	req.Host = "127.0.0.1:27123"
	req.Header.Set("Authorization", "Bearer operator-token")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.EqualValues(t, 1, sink.reads.Load())
}

func TestDoctorAllowsOwnerLocalRequestWithTokenlessPrivateWritesEnabled(t *testing.T) {
	d := openTestDB(t)
	sink := &doctorSink{}
	ts := startTestServer(t, daemon.ServerConfig{
		DB: d.db, StartedAt: d.now, Hooks: sink,
		Auth: config.AuthConfig{AllowUnauthenticatedPrivateNetworkWrites: true},
	})
	req := httptest.NewRequest(http.MethodGet, ts.URL+"/api/v1/doctor", nil)
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.UnixAddr{Name: "/tmp/kata.sock", Net: "unix"}))
	rr := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.EqualValues(t, 1, sink.reads.Load())
}

func TestDoctorAllowsOwnerLocalRequestInInsecureReadonlyMode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		localAddr net.Addr
		remote    string
	}{
		{name: "unix socket", localAddr: &net.UnixAddr{Name: "/tmp/daemon-test.sock", Net: "unix"}},
		{name: "loopback TCP", localAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7777}, remote: "127.0.0.1:54321"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			sink := &doctorSink{}
			ts := startTestServer(t, daemon.ServerConfig{
				DB: d.db, StartedAt: d.now, Hooks: sink, InsecureReadonly: true,
			})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/doctor", nil)
			if tc.remote != "" {
				req.RemoteAddr = tc.remote
			}
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, tc.localAddr))
			rr := httptest.NewRecorder()
			ts.Config.Handler.ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			require.EqualValues(t, 1, sink.reads.Load())
		})
	}
}

type doctorOpaqueSink struct{}

func (doctorOpaqueSink) Enqueue(db.Event) {}

func TestDoctorEndpointDoesNotInventCustomSinkHealth(t *testing.T) {
	d := openTestDB(t)
	ts := startTestServer(t, daemon.ServerConfig{DB: d.db, StartedAt: d.now, Hooks: doctorOpaqueSink{}})
	resp, body := getStatusBody(t, ts, "/api/v1/doctor")
	require.Equal(t, 200, resp.StatusCode)
	var result struct {
		Hooks diagnostics.Hooks `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	require.False(t, result.Hooks.Available)
}
