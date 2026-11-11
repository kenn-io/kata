package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func TestBrowserSessionSurvivesRemoteAttachmentAuthenticationFailure(t *testing.T) {
	store := openAuthTestDB(t)
	project, err := store.CreateProject(t.Context(), "attachments")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "example", Author: "operator"})
	require.NoError(t, err)
	const origin = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	_, err = store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{"attachment.file": []byte(`{"name":"file.pdf","media_type":"application/pdf","size":7,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222","origin":"` + origin + `"}`)}})
	require.NoError(t, err)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			_, _ = fmt.Fprintf(w, `{"version":1,"project_uid":%q,"origin":%q}`, project.UID, origin)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"signature_invalid","message":"private remote detail"}}`))
	}))
	t.Cleanup(upstream.Close)
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true})
	require.NoError(t, err)
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	server := NewServer(ServerConfig{DB: store, WebSessions: manager, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{HubURL: upstream.URL, HubProjectID: 1, Token: "fixture", AllowInsecure: true}}})
	t.Cleanup(func() { _ = server.Close() })
	handler, err := server.HandlerFor(ListenerPolicy{Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true, AllowLocalSession: true})
	require.NoError(t, err)
	issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/kata")
	require.NoError(t, err)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:27123/api/v1/projects/%d/issues/%s/attachments/file/content", project.ID, issue.UID), nil)
	request.AddCookie(manager.Cookie(issued.Cookie))
	request.Header.Set(webSessionHeader, issued.Session)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
	snapshot := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:27123/api/v1/ui/snapshot?view=inbox", nil)
	snapshot.AddCookie(manager.Cookie(issued.Cookie))
	snapshot.Header.Set(webSessionHeader, issued.Session)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, snapshot)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func TestBrowserSessionSurvivesUpstreamLeaseAuthenticationFailure(t *testing.T) {
	store := openAuthTestDB(t)
	project, err := store.CreateProject(t.Context(), "attachments")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "example", Author: "operator"})
	require.NoError(t, err)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/lease") {
			http.Error(w, "unexpected upstream request", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"auth_invalid","message":"private remote detail"}}`))
	}))
	t.Cleanup(upstream.Close)
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true})
	require.NoError(t, err)
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	server := NewServer(ServerConfig{DB: store, WebSessions: manager, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{HubURL: upstream.URL, HubProjectID: 1, Token: "fixture", AllowInsecure: true}}})
	t.Cleanup(func() { _ = server.Close() })
	handler, err := server.HandlerFor(ListenerPolicy{Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true, AllowLocalSession: true})
	require.NoError(t, err)
	issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/kata")
	require.NoError(t, err)
	endpoint := fmt.Sprintf("http://127.0.0.1:27123/api/v1/projects/%d/issues/%s/attachments", project.ID, issue.UID)
	for _, tc := range []struct{ name, path, body, contentType string }{
		{"upload", endpoint + "?name=file.pdf&actor=operator", "file bytes", "application/octet-stream"},
		{"link", endpoint + "/links", `{"path":"/kata/file.pdf","actor":"operator"}`, "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, strings.NewReader(tc.body))
			request.AddCookie(manager.Cookie(issued.Cookie))
			request.Header.Set(webSessionHeader, issued.Session)
			request.Header.Set(webCSRFHeader, issued.CSRF)
			request.Header.Set("Origin", "http://127.0.0.1:27123")
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "hub_credential_rejected")
			require.NotContains(t, response.Body.String(), "private remote detail")
		})
	}
	snapshot := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:27123/api/v1/ui/snapshot?view=inbox", nil)
	snapshot.AddCookie(manager.Cookie(issued.Cookie))
	snapshot.Header.Set(webSessionHeader, issued.Session)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, snapshot)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func TestUIAttachmentCapabilityWithoutLocalVault(t *testing.T) {
	store := openAuthTestDB(t)
	project, err := store.CreateProject(t.Context(), "project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "issue", Author: "operator"})
	require.NoError(t, err)
	cfg := ServerConfig{DB: store}
	require.False(t, effectiveUIPolicy(t.Context(), cfg, issue.UID).Capabilities.Attachments)
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	require.False(t, effectiveUIPolicy(t.Context(), cfg, issue.UID).Capabilities.Attachments)
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator"})
	require.NoError(t, err)
	require.True(t, effectiveUIPolicy(t.Context(), cfg, issue.UID).Capabilities.Attachments)
	require.False(t, effectiveUIPolicy(t.Context(), cfg, "").Capabilities.Attachments)
}

func TestUISnapshotAttachmentCapabilityFollowsSelectedIssueProject(t *testing.T) {
	store := openAuthTestDB(t)
	standalone, err := store.CreateProject(t.Context(), "standalone")
	require.NoError(t, err)
	spoke, err := store.CreateProject(t.Context(), "spoke")
	require.NoError(t, err)
	standaloneIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: standalone.ID, Title: "standalone issue", Author: "operator"})
	require.NoError(t, err)
	spokeIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.ID, Title: "shared issue", Author: "operator"})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: spoke.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: spoke.UID, Enabled: true, PushEnabled: true, Actor: "operator"})
	require.NoError(t, err)
	server := NewServer(ServerConfig{DB: store, UIStore: store})
	t.Cleanup(func() { _ = server.Close() })
	for _, tc := range []struct {
		name     string
		issueUID string
		want     bool
	}{
		{"standalone", standaloneIssue.UID, false},
		{"spoke", spokeIssue.UID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ui/snapshot?view=all-open&selected_issue_uid="+tc.issueUID, nil)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var snapshot struct {
				Capabilities struct {
					Attachments bool `json:"attachments"`
				} `json:"capabilities"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &snapshot))
			assert.Equal(t, tc.want, snapshot.Capabilities.Attachments)
		})
	}
}

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
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:27123/api/v1/ui/session/local", bytes.NewBufferString(`{"return_path":"/kata?view=today"}`))
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
	assert.False(t, session.TokenAuditRead)
}

func TestBootstrapUISessionAdvertisesCredentialAuditWithoutIssueWrites(t *testing.T) {
	manager := newDeterministicSessionManager(t, "https://daemon.example", "instance_a")
	issued, err := manager.IssueSession(Principal{Kind: PrincipalBootstrap}, "/kata?view=credentials")
	require.NoError(t, err)
	response := httptest.NewRecorder()

	issueHTTPSession(response, manager, issued)

	require.Equal(t, http.StatusOK, response.Code)
	var session uiSessionResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &session))
	assert.False(t, session.Writable)
	assert.True(t, session.TokenAuditRead)
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
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://daemon.example/api/v1/ui/session/proxy",
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
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:27123/api/v1/projects", bytes.NewReader(body))
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

type uiCapabilityCredentials struct {
	config.FederationCredentialStore
	credential config.FederationCredential
}

func (c uiCapabilityCredentials) FederationCredential(context.Context, string) (config.FederationCredential, bool, error) {
	return c.credential, true, nil
}
