package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
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
	session, err := newMCPHTTPTestClient().Connect(callCtx, transport, nil)
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
			active, err = newMCPHTTPTestClient().Connect(callCtx, other, nil)
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
	other := newAuthenticatedMCPHTTPTestTransport(endpoint)
	other.HTTPClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.Header.Set("Authorization", "Bearer scoped-token")
		return base.RoundTrip(clone)
	})
	scoped, err := newMCPHTTPTestClient().Connect(callCtx, other, nil)
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
