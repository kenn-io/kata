package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	body, err := json.Marshal(map[string]string{"start_path": t.TempDir(), "actor": "user-a"})
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
}
