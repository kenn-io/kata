package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
)

func TestWebDaemonGatewayAppliesWebLocalProjectPolicyBeforeForwarding(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	handler, manager := newAuthorizedWebDaemonGateway(t, false, true, config.CatalogDaemonConfig{
		Name: "example-remote", URL: upstream.URL, AllowInsecure: true,
	})
	issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/kata")
	require.NoError(t, err)

	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{name: "name only", body: `{"name":"example-project","actor":"user-a"}`, want: http.StatusNoContent},
		{name: "uppercase path", body: `{"START_PATH":" ","actor":"user-a"}`, want: http.StatusForbidden},
		{name: "alias", body: `{"name":"example-project","alias":{"kind":"local"},"actor":"user-a"}`, want: http.StatusForbidden},
		{name: "reassign", body: `{"name":"example-project","reassign":true,"actor":"user-a"}`, want: http.StatusForbidden},
		{name: "replace", body: `{"name":"example-project","replace":true,"actor":"user-a"}`, want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost,
				"http://127.0.0.1:27123/api/v1/ui/proxy/api/v1/projects", bytes.NewBufferString(test.body))
			request.Host = "127.0.0.1:27123"
			request.RemoteAddr = "127.0.0.1:40123"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(webDaemonHeaderName, "example-remote")
			request.Header.Set(webSessionHeader, issued.Session)
			request.Header.Set(webCSRFHeader, issued.CSRF)
			request.Header.Set("Origin", "http://127.0.0.1:27123")
			request.AddCookie(manager.Cookie(issued.Cookie))
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			assert.Equal(t, test.want, response.Code, response.Body.String())
		})
	}
	assert.Equal(t, 1, calls)
}

func TestWebDaemonGatewayIntersectsSnapshotAuthorityAndPreservesConditionalReads(t *testing.T) {
	var snapshotReads int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshotReads++
		if r.Header.Get("If-None-Match") == `"remote-snapshot"` {
			w.Header().Set("ETag", `"remote-snapshot"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"remote-snapshot"`)
		_ = json.NewEncoder(w).Encode(api.UISnapshotResponseBody{
			ContractVersion: api.UISnapshotContractVersion,
			Cursor:          7,
			Capabilities: api.UICapabilities{
				Writable: true, Updates: "sse", ActorPolicy: "request",
			},
		})
	}))
	t.Cleanup(upstream.Close)

	handler, manager := newAuthorizedWebDaemonGateway(t, false, true, config.CatalogDaemonConfig{
		Name: "example-remote", URL: upstream.URL, Token: "target-token", AllowInsecure: true,
	})
	issued, err := manager.IssueSession(Principal{Kind: PrincipalBootstrap}, "/kata")
	require.NoError(t, err)

	first := authorizedGatewayRequest(t, handler, manager, issued, http.MethodGet,
		"/api/v1/ui/snapshot", nil, "")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var snapshot api.UISnapshotResponseBody
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &snapshot))
	assert.False(t, snapshot.Capabilities.Writable)
	assert.Equal(t, "poll", snapshot.Capabilities.Updates)
	gatewayETag := first.Header().Get("ETag")
	require.NotEmpty(t, gatewayETag)
	assert.NotEqual(t, `"remote-snapshot"`, gatewayETag)

	second := authorizedGatewayRequest(t, handler, manager, issued, http.MethodGet,
		"/api/v1/ui/snapshot", nil, gatewayETag)
	assert.Equal(t, http.StatusNotModified, second.Code, second.Body.String())
	assert.Equal(t, gatewayETag, second.Header().Get("ETag"))
	assert.Equal(t, 2, snapshotReads)
}

func TestWebDaemonGatewayRejectsMutationsForNonWritableSourcePrincipals(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	gateway := &webDaemonGateway{
		catalog: []config.CatalogDaemonConfig{{
			Name: "example-remote", URL: upstream.URL, Token: "target-token", AllowInsecure: true,
		}},
		health: make(map[string]webDaemonHealthEntry), inflight: make(map[string]*webDaemonInflightProbe),
		proxies: make(map[string]http.Handler),
	}
	for _, principal := range []Principal{
		{Kind: PrincipalBootstrap},
		{Kind: PrincipalTrustedProxyAbsent},
	} {
		t.Run(string(principal.Kind), func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/7/metadata",
				bytes.NewBufferString(`{}`))
			request.Header.Set(webDaemonHeaderName, "example-remote")
			request.Header.Set("Content-Type", "application/json")
			request = request.WithContext(WithPrincipal(request.Context(), principal))
			response := httptest.NewRecorder()

			gateway.ServeHTTP(response, request)

			assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		})
	}
	assert.Equal(t, 0, calls)
}

func TestWebDaemonGatewayRejectsNestedProxyRoutesWithoutContactingTarget(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	gateway := &webDaemonGateway{
		catalog: []config.CatalogDaemonConfig{{
			Name: "example-remote", URL: upstream.URL, Token: "target-token", AllowInsecure: true,
		}},
		health: make(map[string]webDaemonHealthEntry), inflight: make(map[string]*webDaemonInflightProbe),
		proxies: make(map[string]http.Handler),
	}
	mux := http.NewServeMux()
	mux.Handle(webDaemonProxyPrefix+"/", http.StripPrefix(webDaemonProxyPrefix, gateway))
	request := httptest.NewRequest(http.MethodGet,
		webDaemonProxyPrefix+webDaemonProxyPrefix+pathEventsStreamPath, nil)
	request.Header.Set(webDaemonHeaderName, "example-remote")
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	assert.Equal(t, 0, calls)
}

func TestResolveWebDaemonRequiresCanonicalRootOrigin(t *testing.T) {
	t.Run("canonicalizes origin", func(t *testing.T) {
		resolved := resolveWebDaemon(config.CatalogDaemonConfig{
			Name: "example-remote", URL: "https://DAEMON.EXAMPLE:443/",
		})
		assert.Equal(t, "https://daemon.example", resolved.baseURL)
	})

	t.Run("rejects path prefix", func(t *testing.T) {
		resolved := resolveWebDaemon(config.CatalogDaemonConfig{
			Name: "example-remote", URL: "https://daemon.example/gateway",
		})
		assert.Empty(t, resolved.baseURL)
	})
}

func TestWebDaemonGatewayRejectsNullSnapshotEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "null")
	}))
	t.Cleanup(upstream.Close)

	handler, manager := newAuthorizedWebDaemonGateway(t, false, true, config.CatalogDaemonConfig{
		Name: "example-remote", URL: upstream.URL, AllowInsecure: true,
	})
	issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/kata")
	require.NoError(t, err)

	response := authorizedGatewayRequest(t, handler, manager, issued, http.MethodGet,
		"/api/v1/ui/snapshot", nil, "")

	assert.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
}

func TestWebDaemonGatewayRejectsRemoteStreamsAndCredentialedReadonlyTargets(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"capabilities":{"writable":true,"updates":"sse","actor_policy":"request"}}`)
	}))
	t.Cleanup(upstream.Close)

	handler, manager := newAuthorizedWebDaemonGateway(t, false, true, config.CatalogDaemonConfig{
		Name: "example-remote", URL: upstream.URL, Token: "target-token", AllowInsecure: true,
	})
	issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/kata")
	require.NoError(t, err)
	stream := authorizedGatewayRequest(t, handler, manager, issued, http.MethodGet,
		pathEventsStreamPath, nil, "")
	assert.Equal(t, http.StatusForbidden, stream.Code)

	readonlyHandler, _ := newAuthorizedWebDaemonGateway(t, true, false, config.CatalogDaemonConfig{
		Name: "example-remote", URL: upstream.URL, Token: "target-token", AllowInsecure: true,
	})
	request := httptest.NewRequest(http.MethodGet,
		"http://127.0.0.1:27123/api/v1/ui/proxy/api/v1/ui/snapshot", nil)
	request.Host = "127.0.0.1:27123"
	request.RemoteAddr = "127.0.0.1:40123"
	request.Header.Set(webDaemonHeaderName, "example-remote")
	response := httptest.NewRecorder()
	readonlyHandler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	assert.Equal(t, 0, calls)

	localHandler, _ := newAuthorizedWebDaemonGateway(t, true, false, config.CatalogDaemonConfig{
		Name: "example-local", Local: true,
	})
	request = httptest.NewRequest(http.MethodGet,
		"http://127.0.0.1:27123/api/v1/ui/proxy/api/v1/events/stream", nil)
	request.Host = "127.0.0.1:27123"
	request.RemoteAddr = "127.0.0.1:40123"
	request.Header.Set(webDaemonHeaderName, "example-local")
	response = httptest.NewRecorder()
	localHandler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
}

func TestSharedTCPBearerUISnapshotAcceptsCanonicalPublicAuthority(t *testing.T) {
	store := openAuthTestDB(t)
	manager, err := NewWebSessionManager(WebSessionManagerConfig{
		Origin: "https://daemon.example", InstanceID: "instance_a", Writable: true,
		Auth: config.AuthConfig{Token: "configured-token"}, DB: store,
	})
	require.NoError(t, err)
	server := NewServer(ServerConfig{
		DB: store, StartedAt: time.Now().UTC(), WebSessions: manager,
		Auth: config.AuthConfig{Token: "configured-token"},
	})
	t.Cleanup(func() { _ = server.Close() })
	handler, err := server.HandlerFor(ListenerPolicy{
		Kind: ListenerSharedTCP, Origin: "https://daemon.example", BackendAuthority: "127.0.0.1:7777",
		RequireBrowserSession: true,
	})
	require.NoError(t, err)

	for _, test := range []struct {
		name  string
		token string
		want  int
	}{
		{name: "valid", token: "configured-token", want: http.StatusOK},
		{name: "invalid", token: "wrong-token", want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://daemon.example/api/v1/ui/snapshot?view=all-open", nil)
			request.Host = "daemon.example"
			request.RemoteAddr = "127.0.0.1:40123"
			request.Header.Set("Authorization", "Bearer "+test.token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assert.Equal(t, test.want, response.Code, response.Body.String())
		})
	}
}

func newAuthorizedWebDaemonGateway(
	t *testing.T, insecureReadonly, writable bool, target config.CatalogDaemonConfig,
) (http.Handler, *WebSessionManager) {
	t.Helper()
	store := openAuthTestDB(t)
	manager, err := NewWebSessionManager(WebSessionManagerConfig{
		Origin: "http://127.0.0.1:27123", InstanceID: "instance_a", Writable: writable,
		Updates: "sse", DB: store,
	})
	require.NoError(t, err)
	server := NewServer(ServerConfig{
		DB: store, StartedAt: time.Now().UTC(), WebSessions: manager,
		InsecureReadonly: insecureReadonly, WebDaemons: []config.CatalogDaemonConfig{target},
	})
	t.Cleanup(func() { _ = server.Close() })
	handler, err := server.HandlerFor(ListenerPolicy{
		Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123",
		RequireBrowserSession: true, AllowLocalSession: true,
	})
	require.NoError(t, err)
	return handler, manager
}

func authorizedGatewayRequest(
	t *testing.T,
	handler http.Handler,
	manager *WebSessionManager,
	issued IssuedWebSession,
	method, innerPath string,
	body io.Reader,
	ifNoneMatch string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method,
		"http://127.0.0.1:27123/api/v1/ui/proxy"+innerPath, body)
	request.Host = "127.0.0.1:27123"
	request.RemoteAddr = "127.0.0.1:40123"
	request.Header.Set(webDaemonHeaderName, "example-remote")
	request.Header.Set(webSessionHeader, issued.Session)
	request.Header.Set(webCSRFHeader, issued.CSRF)
	request.Header.Set("Origin", "http://127.0.0.1:27123")
	request.Header.Set("If-None-Match", ifNoneMatch)
	request.AddCookie(manager.Cookie(issued.Cookie))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
