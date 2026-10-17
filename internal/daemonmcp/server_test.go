package daemonmcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	kitdaemon "go.kenn.io/kit/daemon"
)

type roleStore struct {
	db.Storage
	bindings []db.FederationBinding
}

func TestHTTPToolsUseRequestAuthority(t *testing.T) {
	store, cfg, endpoint, url := startTestService(t, true)
	for _, actor := range []string{"example-one", "example-two"} {
		_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
			PlaintextToken: actor, Actor: actor, AdminActor: db.BootstrapActor,
		})
		require.NoError(t, err)
		session := connectHTTP(t, url, actor)
		out, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.create", Arguments: map[string]any{
			"project": "hub-project", "title": "Gateway issue", "idempotency_key": actor,
		}})
		require.NoError(t, err)
		require.False(t, out.IsError, "%+v", out)
		encoded, err := json.Marshal(out.StructuredContent)
		require.NoError(t, err)
		require.Contains(t, string(encoded), actor)
		out, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.load_tokens", Arguments: map[string]any{}})
		require.NoError(t, err)
		require.Equal(t, false, out.StructuredContent.(map[string]any)["available"])
	}
	projects, err := store.ListProjects(t.Context())
	require.NoError(t, err)
	_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: projects[0].UID, Visibility: "teams"}, db.BootstrapActor)
	require.NoError(t, err)
	denied, err := connectHTTP(t, url, "example-one").CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.create", Arguments: map[string]any{
		"project": "hub-project", "title": "Denied issue", "idempotency_key": "denied",
	}})
	require.NoError(t, err)
	require.True(t, denied.IsError)
	token, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "revoked", Actor: "example-agent", AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	_, _, err = store.RevokeAPIToken(t.Context(), token.ID, db.BootstrapActor)
	require.NoError(t, err)
	root, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Scoped root", Author: "example-agent"})
	require.NoError(t, err)
	expires := time.Now().Add(time.Hour)
	_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "scoped", Actor: "example-agent", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: projects[0].UID, RootIssueUID: root.UID}, ExpiresAt: &expires})
	require.NoError(t, err)
	for _, token := range []string{"", "wrong", cfg.Auth.Token, "revoked", "scoped"} {
		req, err := http.NewRequest("POST", url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Contains(t, []int{401, 403}, resp.StatusCode)
		require.Contains(t, resp.Header.Get("Cache-Control"), "no-store")
		_ = resp.Body.Close()
	}
	// A runtime join disables the endpoint even with a valid credential.
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{
		ProjectID: projects[0].ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: projects[0].UID, Actor: "example-one", Enabled: true,
	})
	require.NoError(t, err)
	_, err = New(t.Context(), cfg, endpoint, false, store, http.NotFoundHandler())
	require.Error(t, err)
	req, _ := http.NewRequest("POST", url+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer example-one")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	_ = resp.Body.Close()
	req, _ = http.NewRequest("GET", url+"/api/v1/mcp/capabilities", nil)
	req.Header.Set("Authorization", "Bearer example-one")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var capabilities struct {
		Enabled bool `json:"enabled"`
	}
	require.Equal(t, 200, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&capabilities))
	require.False(t, capabilities.Enabled)
}

func TestHTTPSuccessResponseIsPrivate(t *testing.T) {
	_, cfg, _, url := startTestService(t, false)
	req, _ := http.NewRequest("POST", url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+cfg.Auth.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, 200, response.StatusCode)
	require.Contains(t, response.Header.Get("Cache-Control"), "no-store")
	require.Contains(t, response.Header.Get("Cache-Control"), "private")
}

func TestHTTPStaticScopeAndTransport(t *testing.T) {
	store, cfg, _, url := startTestService(t, false, func(c *config.DaemonConfig, _ kitdaemon.Endpoint) {
		c.MCP.Projects = []string{"hub-project"}
	})
	session := connectHTTP(t, url, cfg.Auth.Token)
	projects, err := store.ListProjects(t.Context())
	require.NoError(t, err)
	_, err = store.RenameProject(t.Context(), projects[0].ID, "renamed-project")
	require.NoError(t, err)
	_, err = store.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	out, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.create", Arguments: map[string]any{
		"project": "renamed-project", "title": "Authoritative issue", "idempotency_key": "static-create",
	}})
	require.NoError(t, err)
	require.False(t, out.IsError, "%+v", out)
	encoded, err := json.Marshal(out.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(encoded), cfg.MCP.Actor)
	retried, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.create", Arguments: map[string]any{
		"project": "renamed-project", "title": "Authoritative issue", "idempotency_key": "static-create",
	}})
	require.NoError(t, err)
	require.False(t, retried.IsError, "%+v", retried)
	require.Equal(t, out.StructuredContent.(map[string]any)["issue"], retried.StructuredContent.(map[string]any)["issue"])
	out, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{"project": "hub-project"}})
	require.NoError(t, err)
	require.True(t, out.IsError)
	for _, attack := range []string{"origin", "host", "oversized"} {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
		if attack == "oversized" {
			body = strings.Repeat(" ", 8<<20) + body
		}
		req, _ := http.NewRequest("POST", url+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+cfg.Auth.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if attack == "origin" {
			req.Header.Set("Origin", "https://other.example")
		}
		if attack == "host" {
			req.Host = "other.example"
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		require.GreaterOrEqual(t, resp.StatusCode, 400, attack)
	}
}

func TestHTTPProxyActorAndEventWait(t *testing.T) {
	store, cfg, _, url := startTestService(t, false, func(c *config.DaemonConfig, ep kitdaemon.Endpoint) {
		c.Auth.Proxy.TrustedActorHeader = "x-test-actor"
		c.Auth.Proxy.TrustedProxyListeners = []string{ep.Address}
	})
	req, _ := http.NewRequest("POST", url+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Auth.Token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, 403, resp.StatusCode)
	_ = resp.Body.Close()
	session := connectHTTP(t, url, cfg.Auth.Token, http.Header{"X-Test-Actor": {"proxy-agent"}})
	before, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	projects, err := store.ListProjects(t.Context())
	require.NoError(t, err)
	created := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Stream event", Author: "proxy-agent"})
		created <- err
	}()
	out, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.events", Arguments: map[string]any{
		"project": "hub-project", "mode": "wait", "after": before, "wait_seconds": 5,
	}})
	require.NoError(t, <-created)
	require.NoError(t, err)
	require.False(t, out.IsError, "%+v", out)
	encoded, err := json.Marshal(out.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "Stream event")
	out, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.create", Arguments: map[string]any{
		"project": "hub-project", "title": "Proxy issue", "idempotency_key": "proxy-create",
	}})
	require.NoError(t, err)
	require.False(t, out.IsError, "%+v", out)
	encoded, err = json.Marshal(out.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "proxy-agent")
}

func startTestService(t *testing.T, identity bool, configure ...func(*config.DaemonConfig, kitdaemon.Endpoint)) (db.Storage, *config.DaemonConfig, kitdaemon.Endpoint, string) {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	cfg := &config.DaemonConfig{MCP: config.MCPConfig{Enabled: true, Actor: "configured-agent"},
		Auth: config.AuthConfig{Token: "daemon-token", RequireTokenIdentity: identity},
		Web:  config.WebConfig{PublicOrigin: "https://daemon.example"}}
	ts := httptest.NewUnstartedServer(nil)
	endpoint := kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: ts.Listener.Addr().String()}
	for _, configure := range configure {
		configure(cfg, endpoint)
	}
	var server *daemon.Server
	service, err := New(t.Context(), cfg, endpoint, false, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.Handler().ServeHTTP(w, r)
	}))
	require.NoError(t, err)
	server = daemon.NewServer(daemon.ServerConfig{DB: store, Auth: cfg.Auth, MCP: service, StartedAt: time.Now()})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	handler, err := server.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerSharedTCP, Origin: "http://" + endpoint.Address})
	require.NoError(t, err)
	ts.Config.Handler = handler
	ts.Start()
	t.Cleanup(ts.Close)
	return store, cfg, endpoint, ts.URL
}

type bearerTransport struct {
	token string
	extra http.Header
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	for key, values := range b.extra {
		r.Header[key] = values
	}
	return http.DefaultTransport.RoundTrip(r)
}

func connectHTTP(t *testing.T, base, token string, extra ...http.Header) *sdkmcp.ClientSession {
	t.Helper()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil)
	transport := bearerTransport{token: token}
	if len(extra) != 0 {
		transport.extra = extra[0]
	}
	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: base + "/mcp",
		HTTPClient: &http.Client{Transport: transport}}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func (s *roleStore) ListFederationBindings(context.Context) ([]db.FederationBinding, error) {
	return s.bindings, nil
}

func TestStartupEligibility(t *testing.T) {
	for _, tt := range []struct {
		name, address, origin                                           string
		spoke, pending, readonly, noToken, noActor, disabled, wantError bool
	}{
		{name: "standalone", address: "100.64.0.5:7777"},
		{name: "proxy backend", address: "127.0.0.1:7777", origin: "https://daemon.example"},
		{name: "plain local", address: "127.0.0.1:7777", wantError: true},
		{name: "socket", wantError: true},
		{name: "spoke", address: "100.64.0.5:7777", spoke: true, wantError: true},
		{name: "pending spoke", address: "100.64.0.5:7777", pending: true, wantError: true},
		{name: "readonly", address: "100.64.0.5:7777", readonly: true, wantError: true},
		{name: "no bearer", address: "100.64.0.5:7777", noToken: true, wantError: true},
		{name: "no actor", address: "100.64.0.5:7777", noActor: true, wantError: true},
		{name: "disabled spoke", spoke: true, disabled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.DaemonConfig{MCP: config.MCPConfig{Enabled: !tt.disabled, Actor: "example-agent"},
				Auth: config.AuthConfig{Token: "daemon-token", TrustPrivateNetwork: true}, Web: config.WebConfig{PublicOrigin: tt.origin}}
			if tt.noToken {
				cfg.Auth.Token = ""
			}
			if tt.noActor {
				cfg.MCP.Actor = ""
			}
			if tt.pending {
				cfg.Federation.Projects = []config.FederationProjectConfig{{Hub: "example-hub"}}
			}
			store := &roleStore{bindings: []db.FederationBinding{{Role: db.FederationRoleHub, Enabled: true}}}
			if tt.name == "standalone" {
				store.bindings = nil
			}
			if tt.spoke {
				store.bindings = append(store.bindings, db.FederationBinding{Role: db.FederationRoleSpoke})
			}
			ep := kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: tt.address}
			if tt.address == "" {
				ep.Network = kitdaemon.NetworkUnix
			}
			service, err := New(t.Context(), cfg, ep, tt.readonly, store, http.NotFoundHandler())
			if tt.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tt.disabled {
				require.Nil(t, service)
				return
			}
			require.NotNil(t, service)
			ok, err := service.Available(t.Context())
			require.NoError(t, err)
			require.True(t, ok)
			store.bindings = []db.FederationBinding{{Role: db.FederationRoleSpoke}}
			ok, err = service.Available(t.Context())
			require.NoError(t, err)
			require.False(t, ok)
		})
	}
}
