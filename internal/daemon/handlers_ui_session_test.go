package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
)

func TestIssueLocalUISession(t *testing.T) {
	store := openAuthTestDB(t)
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	server := NewServer(ServerConfig{
		DB: store, StartedAt: time.Now().UTC(), WebSessions: manager,
	})
	defer func() { _ = server.Close() }()
	handler, err := server.HandlerFor(ListenerPolicy{
		Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123",
		RequireBrowserSession: true, AllowLocalSession: true,
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:27123/api/v1/ui/session/local", bytes.NewBufferString(`{"return_path":"/kata?view=today"}`))
	request.Host = "127.0.0.1:27123"
	request.RemoteAddr = "127.0.0.1:40123"
	request.Header.Set("Origin", "http://127.0.0.1:27123")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.NotEmpty(t, response.Header().Get("Set-Cookie"))
	var session uiSessionResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &session))
	assert.NotEmpty(t, session.Session)
	assert.NotEmpty(t, session.CSRF)
	assert.Equal(t, "/kata?view=today", session.ReturnPath)
	assert.True(t, session.Writable)
}

func TestIssueTrustedProxyUISession(t *testing.T) {
	store := openAuthTestDB(t)
	manager := newDeterministicSessionManager(t, "https://daemon.example", "instance_a")
	server := NewServer(ServerConfig{
		DB: store, StartedAt: time.Now().UTC(), WebSessions: manager,
		Auth: config.AuthConfig{Proxy: config.ProxyConfig{
			TrustedActorHeader:    "X-Kata-Actor",
			TrustedProxyListeners: []string{"127.0.0.1:27123"},
		}},
	})
	defer func() { _ = server.Close() }()
	handler, err := server.HandlerFor(ListenerPolicy{
		Kind: ListenerBrowser, Origin: "https://daemon.example", RequireBrowserSession: true,
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "https://daemon.example/api/v1/ui/session/proxy",
		bytes.NewBufferString(`{"return_path":"/kata?view=today"}`))
	request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 27123}))
	request.Host = "daemon.example"
	request.RemoteAddr = "127.0.0.1:40123"
	request.Header.Set("Origin", "https://daemon.example")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Kata-Actor", "user-a")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var session uiSessionResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &session))
	assert.NotEmpty(t, session.Session)
	assert.NotEmpty(t, session.CSRF)
	assert.Equal(t, "identity", session.ActorPolicy)
}

func TestWebLocalSessionRejectsPathBasedProjectInit(t *testing.T) {
	store := openAuthTestDB(t)
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	server := NewServer(ServerConfig{
		DB: store, StartedAt: time.Now().UTC(), WebSessions: manager,
	})
	defer func() { _ = server.Close() }()
	handler, err := server.HandlerFor(ListenerPolicy{
		Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123",
		RequireBrowserSession: true, AllowLocalSession: true,
	})
	require.NoError(t, err)
	issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/kata")
	require.NoError(t, err)
	for _, tc := range []struct {
		name      string
		startPath string
	}{
		{name: "filesystem path", startPath: t.TempDir()},
		{name: "whitespace-only path", startPath: " "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"start_path": tc.startPath, "actor": "user-a"})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:27123/api/v1/projects", bytes.NewReader(body))
			request.Host = "127.0.0.1:27123"
			request.RemoteAddr = "127.0.0.1:40123"
			request.Header.Set("Origin", "http://127.0.0.1:27123")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(webSessionHeader, issued.Session)
			request.Header.Set(webCSRFHeader, issued.CSRF)
			request.AddCookie(manager.Cookie(issued.Cookie))
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		})
	}
}
