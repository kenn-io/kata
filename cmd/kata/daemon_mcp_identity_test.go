package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestDaemonMCPIdentityStartup(t *testing.T) {
	deploymentHome(t)
	for _, source := range []string{"", "file", "env"} {
		t.Run(source, func(t *testing.T) {
			cfg := config.DaemonConfig{
				Auth: config.AuthConfig{Token: "bootstrap-token", RequireTokenIdentity: true},
				MCP:  config.MCPConfig{Enabled: true, AllProjects: true, Actor: "ignored-actor"},
			}
			if source == "file" {
				cfg.MCP.TokenFile = "/unused/mcp-token"
			}
			if source == "env" {
				cfg.MCP.TokenEnv = "UNUSED_MCP_TOKEN"
			}
			token, err := resolveDaemonMCPStartup(t.Context(), &cfg, kitdaemon.Endpoint{Network: "tcp", Address: "127.0.0.1:7777"})
			if source != "" {
				require.ErrorContains(t, err, "identity mode uses caller tokens")
				return
			}
			require.NoError(t, err)
			require.Empty(t, token)
		})
	}
}

// Contract: the mount uses each caller's credential for reads and writes,
// never the bootstrap token or a caller-selected actor. Sessions cannot be
// reused under another credential, even when both tokens name the same actor.
func TestDaemonMCPIdentityRequests(t *testing.T) {
	home := deploymentHome(t)
	store := openKataTestDB(t, filepath.Join(home, "kata.db"))
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	var firstToken db.APIToken
	for _, grant := range []struct{ token, actor string }{
		{"member-one-token", "member-one"}, {"member-two-token", "member-two"}, {"same-actor-token", "member-one"},
	} {
		token, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: grant.token, Actor: grant.actor, AdminActor: db.BootstrapActor})
		require.NoError(t, err)
		if grant.token == "member-one-token" {
			firstToken = token
		}
	}
	root, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Granted root", Author: "seed-author"})
	require.NoError(t, err)
	hidden, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Outside grant", Author: "seed-author"})
	require.NoError(t, err)
	expires := time.Now().Add(time.Hour)
	_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "scoped-token", Actor: "member-one", AdminActor: db.BootstrapActor, ExpiresAt: &expires,
		Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID},
	})
	require.NoError(t, err)
	expired := time.Now().Add(-time.Hour)
	_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "expired-token", Actor: "member-one", AdminActor: db.BootstrapActor, ExpiresAt: &expired,
	})
	require.NoError(t, err)
	auth := config.AuthConfig{Token: "bootstrap-token", RequireTokenIdentity: true}
	backend := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: auth})
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	rest := httptest.NewServer(backend.Handler())
	t.Cleanup(rest.Close)
	address, err := url.Parse(rest.URL)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler, err := newDaemonMCPHandler(ctx, store, config.MCPConfig{
		Actor: "configured-impostor", Projects: []string{project.Name},
		StartupSections: []string{"issue_discovery", "issue_mutation", "projects"},
	}, "", kitdaemon.Endpoint{Network: "tcp", Address: address.Host}, auth)
	require.NoError(t, err)
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)
	endpoint := gateway.URL + "/mcp"
	for _, token := range []string{"", "invalid-token", "bootstrap-token", "expired-token"} {
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.Equal(t, http.StatusUnauthorized, response.Code, token)
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	t.Cleanup(base.CloseIdleConnections)
	var sessionID atomic.Value
	transport := newAuthenticatedMCPHTTPTestTransport(endpoint)
	transport.HTTPClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.Header.Set("Authorization", "Bearer member-one-token")
		clone.Header.Set("X-Kata-Actor", "header-impostor")
		response, err := base.RoundTrip(clone)
		if err == nil && response.Header.Get("Mcp-Session-Id") != "" {
			sessionID.Store(response.Header.Get("Mcp-Session-Id"))
		}
		return response, err
	})
	callCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	session, err := newMCPHTTPTestClient().Connect(callCtx, transport, &sdkmcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	require.NoError(t, err)
	defer func() { _ = session.Close() }()
	require.Contains(t, session.InitializeResult().Instructions, "actor member-one.")
	for i, actor := range []string{"member-one", "member-two"} {
		active := session
		if i == 1 {
			other := newAuthenticatedMCPHTTPTestTransport(endpoint)
			other.HTTPClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				clone := request.Clone(request.Context())
				clone.Header.Set("Authorization", "Bearer member-two-token")
				return base.RoundTrip(clone)
			})
			active, err = newMCPHTTPTestClient().Connect(callCtx, other, &sdkmcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
			require.NoError(t, err)
			defer func() { _ = active.Close() }()
		}
		created, err := active.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.create", Arguments: map[string]any{
			"project": project.Name, "title": actor + " issue", "idempotency_key": actor + "-issue",
		}})
		require.NoError(t, err)
		require.False(t, created.IsError, "%v", created.Content)
		issue := created.StructuredContent.(map[string]any)["issue"].(map[string]any)
		require.Equal(t, actor, issue["author"])
		listed, err := active.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{"project": project.Name}})
		require.NoError(t, err)
		require.False(t, listed.IsError, "%v", listed.Content)
		outside, err := active.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{"project": "other-project"}})
		require.NoError(t, err)
		require.True(t, outside.IsError)
	}
	spoofed, err := session.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.create", Arguments: map[string]any{
		"project": project.Name, "title": "Spoofed issue", "actor": "argument-impostor", "idempotency_key": "spoofed-issue",
	}})
	require.NoError(t, err)
	require.True(t, spoofed.IsError)
	for _, token := range []string{"member-two-token", "same-actor-token", "scoped-token"} {
		for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPost} {
			request, err := http.NewRequestWithContext(callCtx, method, endpoint, nil)
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Mcp-Session-Id", sessionID.Load().(string))
			request.Header.Set("Accept", "application/json, text/event-stream")
			request.Header.Set("Content-Type", "application/json")
			response, err := (&http.Client{Transport: base}).Do(request)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusForbidden, response.StatusCode, "%s with %s", method, token)
		}
	}
	// A modern request ignores legacy session state and uses its verified
	// credential, including another token for the same actor.
	for _, grant := range []struct{ token, actor string }{
		{"member-two-token", "member-two"}, {"same-actor-token", "member-one"},
	} {
		status, discovery := mcpRPCWithBearer(t, endpoint, grant.token, "server/discover", map[string]any{}, func(r *http.Request) { r.Header.Set("Mcp-Session-Id", sessionID.Load().(string)) })
		require.Equal(t, http.StatusOK, status)
		require.NotContains(t, discovery, "error", "%v", discovery)
		result := discovery["result"].(map[string]any)
		require.Contains(t, result["instructions"], "actor "+grant.actor+".")
	}
	other := newAuthenticatedMCPHTTPTestTransport(endpoint)
	other.HTTPClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.Header.Set("Authorization", "Bearer scoped-token")
		return base.RoundTrip(clone)
	})
	scoped, err := newMCPHTTPTestClient().Connect(callCtx, other, &sdkmcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	require.NoError(t, err)
	defer func() { _ = scoped.Close() }()
	for _, tc := range []struct {
		ref     string
		allowed bool
	}{{project.Name + "#" + root.ShortID, true}, {project.Name + "#" + hidden.ShortID, false}} {
		shown, err := scoped.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.show", Arguments: map[string]any{"ref": tc.ref}})
		require.NoError(t, err)
		require.Equal(t, !tc.allowed, shown.IsError, "%#v", shown.Content)
	}
	denied, err := scoped.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.create", Arguments: map[string]any{
		"project": project.Name, "title": "Outside subtree", "idempotency_key": "outside-subtree",
	}})
	require.NoError(t, err)
	require.True(t, denied.IsError)
	_, _, err = store.RevokeAPIToken(t.Context(), firstToken.ID, db.BootstrapActor)
	require.NoError(t, err)
	_, err = session.ListTools(callCtx, nil)
	require.ErrorContains(t, err, "Unauthorized")
	request := httptest.NewRequestWithContext(ctx, http.MethodDelete, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer member-one-token")
	request.Header.Set("Mcp-Session-Id", sessionID.Load().(string))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	issues, err := store.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, issues, 4)
	authors := make(map[string]string)
	for _, issue := range issues {
		authors[issue.Title] = issue.Author
	}
	require.Equal(t, map[string]string{
		"Granted root": "seed-author", "Outside grant": "seed-author", "member-one issue": "member-one", "member-two issue": "member-two",
	}, authors)
}

// Contract: ending an identity session or modern request releases its REST
// connections. The daemon's Unix listener never times out idle connections,
// so anything cleanup keeps open stays open.
func TestDaemonMCPIdentitySessionsAndRequestsReleaseDaemonConnections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are not supported on Windows")
	}
	home := deploymentHome(t)
	store := openKataTestDB(t, filepath.Join(home, "kata.db"))
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "member-one-token", Actor: "member-one", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	auth := config.AuthConfig{Token: "bootstrap-token", RequireTokenIdentity: true}
	backend := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: auth})
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	dir, err := os.MkdirTemp("", "kmcp")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	var open, issueReads atomic.Int64
	listPath := fmt.Sprintf("/api/v1/projects/%d/issues", project.ID)
	rest := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == listPath {
				issueReads.Add(1)
			}
			backend.Handler().ServeHTTP(w, r)
		}), ReadHeaderTimeout: 10 * time.Second,
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				open.Add(1)
			case http.StateClosed, http.StateHijacked:
				open.Add(-1)
			}
		},
	}
	go func() { _ = rest.Serve(listener) }()
	t.Cleanup(func() { _ = rest.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler, err := newDaemonMCPHandler(ctx, store, config.MCPConfig{
		Projects: []string{project.Name}, StartupSections: []string{"issue_discovery"},
	}, "", kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: socket}, auth)
	require.NoError(t, err)
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	t.Cleanup(base.CloseIdleConnections)
	callCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	for range 4 {
		transport := newAuthenticatedMCPHTTPTestTransport(gateway.URL + "/mcp")
		transport.HTTPClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			clone := request.Clone(request.Context())
			clone.Header.Set("Authorization", "Bearer member-one-token")
			return base.RoundTrip(clone)
		})
		session, err := newMCPHTTPTestClient().Connect(callCtx, transport, &sdkmcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
		require.NoError(t, err)
		listed, err := session.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{"project": project.Name}})
		require.NoError(t, err)
		require.False(t, listed.IsError, "%v", listed.Content)
		require.Positive(t, open.Load(), "the session's tool call reaches the daemon")
		require.NoError(t, session.Close())
	}
	for range 4 {
		before := issueReads.Load()
		status, listed := mcpRPCWithBearer(t, gateway.URL+"/mcp", "member-one-token", "tools/call", map[string]any{"name": "kata.list", "arguments": map[string]any{"project": project.Name}})
		require.Equal(t, http.StatusOK, status)
		require.NotContains(t, listed, "error", "%v", listed)
		require.NotEqual(t, true, listed["result"].(map[string]any)["isError"])
		require.Greater(t, issueReads.Load(), before, "the modern tool call reached the REST backend")
	}
	//nolint:kennlint // sleeptest: session and request cleanup close REST sockets asynchronously.
	require.Eventually(t, func() bool { return open.Load() == 0 }, 5*time.Second, 10*time.Millisecond,
		"closed MCP sessions or requests left daemon connections open")
}

// A successful modern request must leave enough lifetime for its activity report.
func TestDaemonMCPIdentityModernDrainsActivityAfterResponse(t *testing.T) {
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")
	home := deploymentHome(t)
	store := openKataTestDB(t, filepath.Join(home, "kata.db"))
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "member-token", Actor: "example-agent", AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		capture := &cliUseTelemetry{}
		auth := config.AuthConfig{Token: "bootstrap-token", RequireTokenIdentity: true}
		backend := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: auth, Telemetry: capture})
		t.Cleanup(func() { require.NoError(t, backend.Close()) })
		started, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		rest := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/ui/telemetry":
				// Consume the body so the server can observe a client disconnect.
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				once.Do(func() { close(started) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			case "/api/v1/projects":
				select {
				case <-started:
				case <-r.Context().Done():
					return
				}
			}
			backend.Handler().ServeHTTP(w, r)
		}))
		original := http.DefaultTransport
		http.DefaultTransport = rest.Client().Transport
		defer func() { http.DefaultTransport = original }()
		handler, err := newDaemonMCPHandler(t.Context(), store, config.MCPConfig{AllProjects: true, StartupSections: []string{"projects"}}, "", kitdaemon.Endpoint{Network: "tcp", Address: "127.0.0.1:7777"}, auth)
		require.NoError(t, err)
		callCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		request := httptest.NewRequestWithContext(callCtx, http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kata.projects","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`))
		request.Header.Set("Authorization", "Bearer member-token")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Mcp-Protocol-Version", currentMCPProtocolVersion)
		request.Header.Set("Mcp-Method", "tools/call")
		request.Header.Set("Mcp-Name", "kata.projects")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		cancel() // net/http cancels the request context when ServeHTTP returns.
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.NotContains(t, response.Body.String(), `"isError":true`)
		require.NotContains(t, response.Body.String(), `"error":`)
		// Settle cancellation at both ends before allowing delayed delivery.
		synctest.Wait()
		close(release)
		synctest.Wait()
		capture.mu.Lock()
		defer capture.mu.Unlock()
		require.Equal(t, []string{"agent_active"}, capture.events)
	})
}
