package daemon

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func TestBrowserSession(t *testing.T) {
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	session, err := manager.IssueSession(Principal{Kind: PrincipalStaticToken}, "/views/inbox")
	require.NoError(t, err)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := PrincipalFromContext(r.Context())
		assert.True(t, ok)
		w.WriteHeader(http.StatusNoContent)
	})
	handler := requireBrowserSession(manager, ListenerPolicy{
		Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true,
		AllowLocalSession: true,
	}, next)

	tests := []struct {
		name   string
		cookie string
		header string
		want   int
	}{
		{name: "cookie only", cookie: session.Cookie, want: http.StatusUnauthorized},
		{name: "header only", header: session.Session, want: http.StatusUnauthorized},
		{name: "mismatch", cookie: session.Cookie, header: "mismatch", want: http.StatusUnauthorized},
		{name: "pair", cookie: session.Cookie, header: session.Session, want: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:27123/api/v1/events/stream", nil)
			if tt.cookie != "" {
				request.AddCookie(manager.Cookie(tt.cookie))
			}
			request.Header.Set(webSessionHeader, tt.header)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assert.Equal(t, tt.want, response.Code)
			if tt.want == http.StatusUnauthorized {
				assert.Equal(t, "loopback", response.Header().Get("X-Kata-Web-Authentication"))
			}
		})
	}
}

func TestWebLocalSessionIsLimitedToSPAOperations(t *testing.T) {
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/kata")
	require.NoError(t, err)
	handler := requireBrowserSession(manager, ListenerPolicy{
		Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true,
		AllowLocalSession: true,
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, test := range []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{name: "snapshot", method: http.MethodGet, path: "/api/v1/ui/snapshot", want: http.StatusNoContent},
		{name: "daemon roster", method: http.MethodGet, path: "/api/v1/ui/daemons", want: http.StatusNoContent},
		{name: "proxied snapshot", method: http.MethodGet, path: "/api/v1/ui/proxy/api/v1/ui/snapshot", want: http.StatusNoContent},
		{name: "proxied project metadata", method: http.MethodPost, path: "/api/v1/ui/proxy/api/v1/projects/7/metadata", want: http.StatusNoContent},
		{name: "proxied federation", method: http.MethodPost, path: "/api/v1/ui/proxy/api/v1/federation/replicas", want: http.StatusForbidden},
		{name: "project creation", method: http.MethodPost, path: "/api/v1/projects", want: http.StatusNoContent},
		{name: "project metadata", method: http.MethodPost, path: "/api/v1/projects/7/metadata", want: http.StatusNoContent},
		{name: "issue edit", method: http.MethodPatch, path: "/api/v1/projects/7/issues/abc4", want: http.StatusNoContent},
		{name: "recurrence deletion", method: http.MethodDelete, path: "/api/v1/projects/7/recurrences/01J00000000000000000000001", want: http.StatusNoContent},
		{name: "federation", method: http.MethodPost, path: "/api/v1/federation/replicas", want: http.StatusForbidden},
		{name: "project purge", method: http.MethodPost, path: "/api/v1/projects/7/actions/purge", want: http.StatusForbidden},
		{name: "issue purge", method: http.MethodPost, path: "/api/v1/projects/7/issues/abc4/actions/purge", want: http.StatusForbidden},
		{name: "integration", method: http.MethodPost, path: "/api/v1/projects/7/issue-sync/github/enable", want: http.StatusForbidden},
		{name: "path resolution", method: http.MethodPost, path: "/api/v1/projects/resolve", want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "http://127.0.0.1:27123"+test.path, nil)
			request.AddCookie(manager.Cookie(issued.Cookie))
			request.Header.Set(webSessionHeader, issued.Session)
			request.Header.Set(webCSRFHeader, issued.CSRF)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			assert.Equal(t, test.want, response.Code)
		})
	}
}

func TestSharedTCPEventStreamPreservesCLIAuthentication(t *testing.T) {
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	manager.auth.Token = "configured-token"
	handler := requireBrowserSession(manager, ListenerPolicy{
		Kind: ListenerSharedTCP, Origin: "https://daemon.example", BackendAuthority: "127.0.0.1:7777",
		RequireBrowserSession: true,
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodGet, "https://daemon.example/api/v1/events/stream", nil)
	request.Header.Set("Authorization", "Bearer configured-token")
	request.Header.Set("Accept", "text/event-stream")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	assert.Equal(t, http.StatusNoContent, response.Code)
}

func TestSharedTCPTokenlessPublicAuthorityRequiresBrowserSession(t *testing.T) {
	manager := newDeterministicSessionManager(t, "https://daemon.example", "instance_a")
	policy := ListenerPolicy{
		Kind: ListenerSharedTCP, Origin: "https://daemon.example", BackendAuthority: "127.0.0.1:7777",
		AllowedHosts: []string{"backend.example:7777"}, RequireBrowserSession: true,
	}
	handler := requireBrowserSession(manager, policy,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	publicRequest := httptest.NewRequest(http.MethodGet, "https://daemon.example/api/v1/projects", nil)
	publicResponse := httptest.NewRecorder()
	handler.ServeHTTP(publicResponse, publicRequest)
	assert.Equal(t, http.StatusUnauthorized, publicResponse.Code)
	assert.Equal(t, "https://daemon.example", publicResponse.Header().Get("X-Kata-Web-Origin"))

	for _, authority := range []string{"127.0.0.1:7777", "localhost:7777", "backend.example:7777"} {
		t.Run(authority, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://"+authority+"/api/v1/projects", nil)
			request.RemoteAddr = "127.0.0.1:54321"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assert.Equal(t, http.StatusNoContent, response.Code)
		})
	}
}

func TestSharedTCPExplicitUnauthenticatedPrivateNetworkWritesPreservesCLI(t *testing.T) {
	manager := newDeterministicSessionManager(t, "http://100.64.0.5:7777", "instance_a")
	manager.auth.AllowUnauthenticatedPrivateNetworkWrites = true
	handler := requireBrowserSession(manager, ListenerPolicy{
		Kind: ListenerSharedTCP, Origin: "https://daemon.example", BackendAuthority: "100.64.0.5:7777",
		RequireBrowserSession: true,
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodPost, "http://100.64.0.5:7777/api/v1/projects", nil)
	request.RemoteAddr = "100.64.0.6:54321"
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	assert.Equal(t, http.StatusNoContent, response.Code)
}

func TestBrowserSessionsRemainIndependentAcrossTabs(t *testing.T) {
	entropy := bytes.Join([][]byte{
		bytes.Repeat([]byte{0x61}, 32),
		bytes.Repeat([]byte{0x62}, 32),
		bytes.Repeat([]byte{0x63}, 32),
		bytes.Repeat([]byte{0x64}, 32),
		bytes.Repeat([]byte{0x65}, 32),
	}, nil)
	manager, err := NewWebSessionManager(WebSessionManagerConfig{
		Origin:     "http://127.0.0.1:27123",
		InstanceID: "instance_a",
		Entropy:    bytes.NewReader(entropy),
		Writable:   true,
	})
	require.NoError(t, err)
	first, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/views/inbox")
	require.NoError(t, err)
	second, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/views/today")
	require.NoError(t, err)
	require.Equal(t, first.Cookie, second.Cookie)

	mux := http.NewServeMux()
	registerUISessionHandlers(mux, manager)
	policy := ListenerPolicy{
		Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true,
	}
	handler := requireBrowserSession(manager, policy, mux)
	logout := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:27123/api/v1/ui/session", nil)
	logout.AddCookie(manager.Cookie(first.Cookie))
	logout.Header.Set(webSessionHeader, first.Session)
	logout.Header.Set(webCSRFHeader, first.CSRF)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, logout)
	require.Equal(t, http.StatusNoContent, response.Code)
	assert.Empty(t, response.Header().Values("Set-Cookie"))

	_, err = manager.Authenticate(context.Background(), first.Cookie, first.Session)
	assert.ErrorIs(t, err, ErrWebSessionInvalid)
	_, err = manager.Authenticate(context.Background(), second.Cookie, second.Session)
	assert.NoError(t, err)
}

func TestBrowserSessionRetainsIdentityAndCapabilities(t *testing.T) {
	store := openAuthTestDB(t)
	tokenRow, _, err := store.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "user-a-token",
		Actor:          "user-a",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	manager, err := NewWebSessionManager(WebSessionManagerConfig{
		Origin:     "http://127.0.0.1:27123",
		InstanceID: "instance_a",
		Entropy:    bytes.NewReader(bytes.Repeat([]byte{0x64}, 32*20)),
		Writable:   true,
		Updates:    "sse",
		Auth:       config.AuthConfig{Token: "bootstrap-token", RequireTokenIdentity: true},
		DB:         store,
	})
	require.NoError(t, err)

	issued, err := manager.Login(context.Background(), "user-a-token", "/views/inbox")
	require.NoError(t, err)
	assert.Equal(t, PrincipalDBToken, issued.Principal.Kind)
	assert.Equal(t, "user-a", issued.Principal.Actor)
	assert.True(t, issued.Writable)
	assert.Equal(t, "sse", issued.Updates)

	_, err = manager.Authenticate(context.Background(), issued.Cookie, issued.Session)
	require.NoError(t, err)
	_, _, err = store.RevokeAPIToken(context.Background(), tokenRow.ID, db.BootstrapActor)
	require.NoError(t, err)
	_, err = manager.Authenticate(context.Background(), issued.Cookie, issued.Session)
	assert.ErrorIs(t, err, ErrWebSessionInvalid)

	bootstrap, err := manager.Login(context.Background(), "bootstrap-token", "/views/inbox")
	require.NoError(t, err)
	assert.Equal(t, PrincipalBootstrap, bootstrap.Principal.Kind)
	assert.False(t, bootstrap.Writable)

	readonly, err := NewWebSessionManager(WebSessionManagerConfig{
		Origin:     "http://127.0.0.1:28123",
		InstanceID: "instance_b",
		Entropy:    bytes.NewReader(bytes.Repeat([]byte{0x65}, 32*20)),
		Writable:   false,
		Updates:    "poll",
	})
	require.NoError(t, err)
	issued, err = readonly.IssueSession(Principal{}, "/views/today")
	require.NoError(t, err)
	assert.False(t, issued.Writable)
	assert.Equal(t, "poll", issued.Updates)
}

func TestEffectiveUIPolicyUsesBrowserSessionPrincipal(t *testing.T) {
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	cfg := ServerConfig{WebSessions: manager}

	bootstrap := effectiveUIPolicy(WithPrincipal(context.Background(), Principal{
		Kind: PrincipalBootstrap,
	}), cfg)
	assert.False(t, bootstrap.Capabilities.Writable)

	local := effectiveUIPolicy(WithPrincipal(context.Background(), Principal{
		Kind: PrincipalWebLocal,
	}), cfg)
	assert.True(t, local.Capabilities.Writable)
}

func TestBrowserSessionExpires(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	manager, err := NewWebSessionManager(WebSessionManagerConfig{
		Origin:     "http://127.0.0.1:27123",
		InstanceID: "instance_a",
		Clock:      func() time.Time { return now },
		Entropy:    bytes.NewReader(bytes.Repeat([]byte{0x67}, 32*20)),
	})
	require.NoError(t, err)
	issued, err := manager.IssueSession(Principal{}, "/views/inbox")
	require.NoError(t, err)

	_, err = manager.Authenticate(context.Background(), issued.Cookie, issued.Session)
	require.NoError(t, err)
	now = now.Add(24 * time.Hour)
	_, err = manager.Authenticate(context.Background(), issued.Cookie, issued.Session)
	assert.ErrorIs(t, err, ErrWebSessionInvalid)
}

func TestBrowserSessionRejectsCrossDaemonReplay(t *testing.T) {
	first := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	second, err := NewWebSessionManager(WebSessionManagerConfig{
		Origin:     "http://127.0.0.1:28123",
		InstanceID: "instance_b",
		Entropy:    bytes.NewReader(bytes.Repeat([]byte{0x62}, 32*20)),
		Writable:   true,
		Updates:    "sse",
	})
	require.NoError(t, err)
	assert.NotEqual(t, first.CookieName(), second.CookieName())

	issued, err := first.IssueSession(Principal{}, "/views/inbox")
	require.NoError(t, err)
	_, err = second.Authenticate(context.Background(), issued.Cookie, issued.Session)
	assert.ErrorIs(t, err, ErrWebSessionInvalid)
}

func TestBrowserMutation(t *testing.T) {
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	session, err := manager.IssueSession(Principal{}, "/views/inbox")
	require.NoError(t, err)
	handler := requireBrowserSession(manager, ListenerPolicy{
		Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true,
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:27123/api/v1/projects", nil)
	request.AddCookie(manager.Cookie(session.Cookie))
	request.Header.Set(webSessionHeader, session.Session)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusForbidden, response.Code)

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:27123/api/v1/projects", nil)
	request.AddCookie(manager.Cookie(session.Cookie))
	request.Header.Set(webSessionHeader, session.Session)
	request.Header.Set(webCSRFHeader, session.CSRF)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusNoContent, response.Code)

	bootstrap, err := manager.IssueSession(Principal{Kind: PrincipalBootstrap}, "/views/inbox")
	require.NoError(t, err)
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:27123/api/v1/projects", nil)
	request.AddCookie(manager.Cookie(bootstrap.Cookie))
	request.Header.Set(webSessionHeader, bootstrap.Session)
	request.Header.Set(webCSRFHeader, bootstrap.CSRF)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusForbidden, response.Code)
}

func TestBrowserSessionLogoutInvalidatesPairAndHasNoRefresh(t *testing.T) {
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	issued, err := manager.IssueSession(Principal{}, "/views/inbox")
	require.NoError(t, err)
	mux := http.NewServeMux()
	registerUISessionHandlers(mux, manager)
	mux.HandleFunc(http.MethodGet+" /api/v1/data", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	policy := ListenerPolicy{
		Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true,
	}
	handler := requireBrowserSession(manager, policy, mux)

	logout := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:27123/api/v1/ui/session", nil)
	logout.AddCookie(manager.Cookie(issued.Cookie))
	logout.Header.Set(webSessionHeader, issued.Session)
	logout.Header.Set(webCSRFHeader, issued.CSRF)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, logout)
	require.Equal(t, http.StatusNoContent, response.Code)

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:27123/api/v1/data", nil)
	request.AddCookie(manager.Cookie(issued.Cookie))
	request.Header.Set(webSessionHeader, issued.Session)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusUnauthorized, response.Code)

	refresh := httptest.NewRequest(http.MethodPost, "/api/v1/ui/session/refresh", nil)
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, refresh)
	assert.Equal(t, http.StatusNotFound, response.Code)
}

func TestBrowserSessionLogoutAllowsReadOnlyAndBootstrapPrincipals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		writable  bool
		principal Principal
	}{
		{name: "read only", principal: Principal{}},
		{name: "bootstrap", writable: true, principal: Principal{Kind: PrincipalBootstrap}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
			manager.writable = tc.writable
			issued, err := manager.IssueSession(tc.principal, "/views/inbox")
			require.NoError(t, err)
			mux := http.NewServeMux()
			registerUISessionHandlers(mux, manager)
			policy := ListenerPolicy{
				Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true,
			}
			handler, err := ApplyListenerPolicy(requireBrowserSession(manager, policy, mux), policy)
			require.NoError(t, err)

			request := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:27123/api/v1/ui/session", nil)
			request.AddCookie(manager.Cookie(issued.Cookie))
			request.Header.Set("Origin", policy.Origin)
			request.Header.Set(webSessionHeader, issued.Session)
			request.Header.Set(webCSRFHeader, issued.CSRF)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			require.Equal(t, http.StatusNoContent, response.Code)
		})
	}
}

func TestStreamEventsBrowser(t *testing.T) {
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	issued, err := manager.IssueSession(Principal{}, "/views/inbox")
	require.NoError(t, err)
	policy := ListenerPolicy{
		Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true,
	}
	handler := requireBrowserSession(manager, policy, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "41", r.Header.Get("Last-Event-ID"))
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:27123/api/v1/events/stream", nil)
	request.AddCookie(manager.Cookie(issued.Cookie))
	request.Header.Set("Last-Event-ID", "41")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusUnauthorized, response.Code)

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:27123/api/v1/events/stream", nil)
	request.AddCookie(manager.Cookie(issued.Cookie))
	request.Header.Set(webSessionHeader, issued.Session)
	request.Header.Set("Last-Event-ID", "41")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusNoContent, response.Code)

	readonly, err := NewWebSessionManager(WebSessionManagerConfig{
		Origin:     "http://127.0.0.1:29123",
		InstanceID: "instance_c",
		Entropy:    bytes.NewReader(bytes.Repeat([]byte{0x63}, 32*20)),
		Writable:   false,
		Updates:    "poll",
	})
	require.NoError(t, err)
	readonlyHandler := requireBrowserSession(readonly, ListenerPolicy{
		Kind: ListenerBrowser, Origin: "http://127.0.0.1:29123", RequireBrowserSession: true,
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:29123/api/v1/events/stream", nil)
	response = httptest.NewRecorder()
	readonlyHandler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestBrowserEventStreamRevalidatesSessionAuthority(t *testing.T) {
	t.Run("logout", func(t *testing.T) {
		manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
		issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/views/inbox")
		require.NoError(t, err)
		ctx := withWebSessionRevalidation(context.Background(), manager, issued.Cookie, issued.Session)
		manager.Logout(issued.Session)
		assertStreamRejectsInvalidatedSession(ctx, t)
	})

	t.Run("expiry", func(t *testing.T) {
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		manager, err := NewWebSessionManager(WebSessionManagerConfig{
			Origin:     "http://127.0.0.1:28123",
			InstanceID: "instance_b",
			Clock:      func() time.Time { return now },
			Entropy:    bytes.NewReader(bytes.Repeat([]byte{0x68}, 32*20)),
			Writable:   true,
		})
		require.NoError(t, err)
		issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/views/inbox")
		require.NoError(t, err)
		ctx := withWebSessionRevalidation(context.Background(), manager, issued.Cookie, issued.Session)
		now = now.Add(webSessionTTL)
		assertStreamRejectsInvalidatedSession(ctx, t)
	})

	t.Run("token revocation", func(t *testing.T) {
		store := openAuthTestDB(t)
		token, _, err := store.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
			PlaintextToken: "user-a-token",
			Actor:          "user-a",
			AdminActor:     db.BootstrapActor,
		})
		require.NoError(t, err)
		manager, err := NewWebSessionManager(WebSessionManagerConfig{
			Origin:     "http://127.0.0.1:29123",
			InstanceID: "instance_c",
			Entropy:    bytes.NewReader(bytes.Repeat([]byte{0x69}, 32*20)),
			Writable:   true,
			Auth:       config.AuthConfig{RequireTokenIdentity: true},
			DB:         store,
		})
		require.NoError(t, err)
		issued, err := manager.Login(context.Background(), "user-a-token", "/views/inbox")
		require.NoError(t, err)
		ctx := withWebSessionRevalidation(context.Background(), manager, issued.Cookie, issued.Session)
		_, _, err = store.RevokeAPIToken(context.Background(), token.ID, db.BootstrapActor)
		require.NoError(t, err)
		assertStreamRejectsInvalidatedSession(ctx, t)
	})
}

func assertStreamRejectsInvalidatedSession(ctx context.Context, t *testing.T) {
	t.Helper()
	response := httptest.NewRecorder()
	messages := make(chan StreamMsg, 1)
	messages <- StreamMsg{Kind: "reset", ResetID: 17}
	runLivePhase(ctx, livePhaseDeps{
		w: response, flusher: response, ch: messages,
	}, 0, 0)
	assert.Empty(t, response.Body.String())
}

func TestBrowserSessionAuthenticatedResponsesArePrivate(t *testing.T) {
	manager := newDeterministicSessionManager(t, "https://daemon.example", "instance_a")
	issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/views/inbox")
	require.NoError(t, err)
	handler := requireBrowserSession(manager, ListenerPolicy{
		Kind: ListenerBrowser, Origin: "https://daemon.example", RequireBrowserSession: true,
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	request := httptest.NewRequest(http.MethodGet, "https://daemon.example/api/v1/ui/snapshot", nil)
	request.AddCookie(manager.Cookie(issued.Cookie))
	request.Header.Set(webSessionHeader, issued.Session)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusNoContent, response.Code)
	assert.Equal(t, "private, no-store", response.Header().Get("Cache-Control"))
	assert.ElementsMatch(t, []string{"Cookie", webSessionHeader}, response.Header().Values("Vary"))
}

func TestBrowserSessionReadonlyIgnoresAmbientCookieButRejectsSessionHeader(t *testing.T) {
	manager, err := NewWebSessionManager(WebSessionManagerConfig{
		Origin:     "http://127.0.0.1:29123",
		InstanceID: "instance_c",
		Entropy:    bytes.NewReader(bytes.Repeat([]byte{0x66}, 32*20)),
		Writable:   false,
		Updates:    "poll",
	})
	require.NoError(t, err)
	issued, err := manager.IssueSession(Principal{}, "/views/inbox")
	require.NoError(t, err)
	handler := requireBrowserSession(manager, ListenerPolicy{
		Kind: ListenerBrowser, Origin: "http://127.0.0.1:29123", RequireBrowserSession: true,
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:29123/api/v1/ui/snapshot", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusNoContent, response.Code)

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:29123/api/v1/ui/snapshot", nil)
	request.AddCookie(manager.Cookie(issued.Cookie))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusNoContent, response.Code)

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:29123/api/v1/ui/snapshot", nil)
	request.Header.Set(webSessionHeader, issued.Session)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestBrowserCookie(t *testing.T) {
	httpManager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	assert.Equal(t, "kata_session_instance_a", httpManager.CookieName())
	assert.False(t, httpManager.Cookie("").Secure)

	httpsManager := newDeterministicSessionManager(t, "https://daemon.example", "instance_b")
	cookie := httpsManager.Cookie("")
	assert.Equal(t, "__Host-kata_session_instance_b", cookie.Name)
	assert.True(t, cookie.Secure)
	assert.Empty(t, cookie.Domain)
	assert.Equal(t, "/", cookie.Path)
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
}

func newDeterministicSessionManager(t *testing.T, origin, instance string) *WebSessionManager {
	t.Helper()
	manager, err := NewWebSessionManager(WebSessionManagerConfig{
		Origin:     origin,
		InstanceID: instance,
		Entropy:    bytes.NewReader(bytes.Repeat([]byte{0x61}, 32*20)),
		Writable:   true,
		Updates:    "sse",
	})
	require.NoError(t, err)
	return manager
}
