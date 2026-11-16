package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/testenv"
	kitdaemon "go.kenn.io/kit/daemon"
)

func mountedEventsFixture(t *testing.T, identity, enabled bool) (*testenv.Env, string) {
	t.Helper()
	opts := []testenv.Option{testenv.WithAuthToken("bootstrap-token")}
	if identity {
		opts = append(opts, testenv.WithRequireTokenIdentity())
	}
	if enabled {
		opts = append(opts, withRunningEventsDaemon(t, newSignedReceiver(t)))
	}
	env := testenv.New(t, opts...)
	for _, grant := range []struct{ token, actor string }{
		{"test-mcp-token", "example-agent"}, {"same-actor-token", "example-agent"},
	} {
		_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: grant.token, Actor: grant.actor, AdminActor: db.BootstrapActor})
		require.NoError(t, err)
	}
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	return env, mountEventsEndpoint(t, env, identity, nil)
}

func mountEventsEndpoint(t *testing.T, env *testenv.Env, identity bool, projects []string, startup ...[]string) string {
	t.Helper()
	sections := []string{"coordination", "projects"}
	if len(startup) > 0 {
		sections = startup[0]
	}
	address, err := url.Parse(env.URL)
	require.NoError(t, err)
	handler, err := newDaemonMCPHandler(t.Context(), env.DB, config.MCPConfig{
		AllProjects: projects == nil, Projects: projects, Actor: "example-agent", StartupSections: sections,
	}, "test-mcp-token", kitdaemon.Endpoint{Network: "tcp", Address: address.Host}, config.AuthConfig{Token: "bootstrap-token", RequireTokenIdentity: identity})
	require.NoError(t, err)
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)
	return gateway.URL + "/mcp"
}

func requireEventError(t *testing.T, wire map[string]any, code int) {
	t.Helper()
	errObj, ok := wire["error"].(map[string]any)
	require.True(t, ok, "%v", wire)
	require.InDelta(t, float64(code), errObj["code"], 0.5)
}

func unsubscribeParams(recipient string, uids []string) map[string]any {
	params := eventsSubscribeParams(recipient, uids)
	delete(params["delivery"].(map[string]any), "secret")
	return params
}

func TestDaemonMCPEventsLifecycle(t *testing.T) {
	for _, identity := range []bool{false, true} {
		t.Run(strconv.FormatBool(identity), func(t *testing.T) {
			env, endpoint := mountedEventsFixture(t, identity, true)
			status, discovery := mcpRPC(t, endpoint, "server/discover", map[string]any{})
			require.Equal(t, http.StatusOK, status)
			require.NotContains(t, discovery, "error")
			capabilities := discovery["result"].(map[string]any)["capabilities"].(map[string]any)
			require.Equal(t, map[string]any{}, capabilities["events"])
			require.Equal(t, map[string]any{}, capabilities["extensions"].(map[string]any)["io.modelcontextprotocol/events"])
			status, listing := mcpRPC(t, endpoint, "events/list", map[string]any{})
			require.Equal(t, http.StatusOK, status)
			require.NotContains(t, listing, "error")
			result := listing["result"].(map[string]any)
			require.Equal(t, "complete", result["resultType"])
			events := result["events"].([]any)
			require.Len(t, events, 1)
			require.Equal(t, mcpevents.EventName, events[0].(map[string]any)["name"])
			project, err := env.DB.ProjectByName(t.Context(), "spoke-project")
			require.NoError(t, err)
			if identity {
				endpoint = mountEventsEndpoint(t, env, identity, []string{project.Name})
				_, refused := mcpRPC(t, endpoint, "events/subscribe", eventsSubscribeParams("example-agent/worker", nil))
				requireEventError(t, refused, mcpevents.CodeForbidden)
			}
			_, accepted := mcpRPC(t, endpoint, "events/subscribe", eventsSubscribeParams("example-agent/worker", []string{project.UID}))
			require.NotContains(t, accepted, "error", "%v", accepted)
			id := accepted["result"].(map[string]any)["id"].(string)
			row, err := env.DB.GetMCPEventSubscription(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, db.MCPEventStateActive, row.State)
			if identity {
				token, err := env.DB.ResolveAPIToken(t.Context(), "test-mcp-token")
				require.NoError(t, err)
				require.Equal(t, strconv.FormatInt(token.ID, 10), row.PrincipalSubject)
			} else {
				require.Equal(t, mcpevents.AuthorityStaticToken, row.PrincipalAuthority)
			}
			if identity {
				_, other := mcpRPCWithBearer(t, endpoint, "same-actor-token", "events/subscribe", eventsSubscribeParams("example-agent/worker", []string{project.UID}))
				require.NotContains(t, other, "error", "%v", other)
				otherID := other["result"].(map[string]any)["id"].(string)
				require.NotEqual(t, id, otherID)
				_, stopped := mcpRPCWithBearer(t, endpoint, "same-actor-token", "events/unsubscribe", unsubscribeParams("example-agent/worker", []string{project.UID}))
				require.NotContains(t, stopped, "error", "%v", stopped)
				row, err = env.DB.GetMCPEventSubscription(t.Context(), id)
				require.NoError(t, err)
				require.Equal(t, db.MCPEventStateActive, row.State, "another token must not end this subscription")
			}
			_, stopped := mcpRPC(t, endpoint, "events/unsubscribe", unsubscribeParams("example-agent/worker", []string{project.UID}))
			require.NotContains(t, stopped, "error", "%v", stopped)
			row, err = env.DB.GetMCPEventSubscription(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, db.MCPEventStateEnded, row.State)
			require.Equal(t, "unsubscribed", row.EndReason)
		})
	}
}

func TestDaemonMCPEventsHeaders(t *testing.T) {
	for _, identity := range []bool{false, true} {
		t.Run(strconv.FormatBool(identity), func(t *testing.T) {
			env, endpoint := mountedEventsFixture(t, identity, true)
			status, wire := mcpRPCWithBearer(t, endpoint, "test-mcp-token", "events/subscribe", eventsSubscribeParams("example-agent/worker", nil), func(r *http.Request) { r.Header.Set("Mcp-Name", "other") })
			require.Equal(t, http.StatusBadRequest, status, "%v", wire)
			requireEventError(t, wire, mcpevents.CodeHeaderMismatch)
			count, err := env.DB.CountActiveMCPEventSubscriptions(t.Context())
			require.NoError(t, err)
			require.Zero(t, count)
		})
	}
}

// Holding eight calls shows another credential or legacy session can proceed.
func TestDaemonMCPEventsAdmission(t *testing.T) {
	for _, mode := range []string{"modern identity", "legacy identity"} {
		t.Run(mode, func(t *testing.T) {
			env, _ := mountedEventsFixture(t, true, false)
			target, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(target)
			entered := make(chan string, 32)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/projects" {
					entered <- r.Header.Get("Authorization")
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(rest.Close)
			t.Cleanup(unblock)
			mounted := *env
			mounted.URL = rest.URL
			endpoint := mountEventsEndpoint(t, &mounted, true, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			legacy := func() *sdkmcp.ClientSession {
				transport := newAuthenticatedMCPHTTPTestTransport(endpoint)
				session, err := newMCPHTTPTestClient().Connect(ctx, transport, &sdkmcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
				require.NoError(t, err)
				t.Cleanup(func() { _ = session.Close() })
				return session
			}
			firstLegacy, secondLegacy := legacy(), legacy()
			var wg sync.WaitGroup
			start := func(bearer string, session *sdkmcp.ClientSession) {
				wg.Go(func() {
					if session != nil {
						result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.projects", Arguments: map[string]any{}})
						if assert.NoError(t, err) {
							assert.False(t, result.IsError, "%v", result.Content)
						}
					} else {
						raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kata.projects","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"` + currentMCPProtocolVersion + `","io.modelcontextprotocol/clientCapabilities":{}}}}`
						request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(raw))
						if !assert.NoError(t, err) {
							return
						}
						request.Header.Set("Authorization", "Bearer "+bearer)
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("Accept", "application/json, text/event-stream")
						request.Header.Set("Mcp-Protocol-Version", currentMCPProtocolVersion)
						request.Header.Set("Mcp-Method", "tools/call")
						request.Header.Set("Mcp-Name", "kata.projects")
						response, err := http.DefaultClient.Do(request)
						if !assert.NoError(t, err) {
							return
						}
						defer func() { _ = response.Body.Close() }()
						var wire map[string]any
						if assert.NoError(t, json.NewDecoder(response.Body).Decode(&wire)) {
							assert.Equal(t, http.StatusOK, response.StatusCode)
							assert.NotContains(t, wire, "error", "%v", wire)
						}
					}
				})
			}
			next := func() string {
				select {
				case token := <-entered:
					return token
				case <-ctx.Done():
					t.Fatal("tool did not reach REST backend")
					return ""
				}
			}
			for range 8 {
				if mode == "legacy identity" {
					start("", firstLegacy)
				} else {
					start("test-mcp-token", nil)
				}
			}
			for range 8 {
				next()
			}
			switch mode {
			case "legacy identity":
				start("", secondLegacy) // another session for the same token is independent
				next()
			default:
				start("same-actor-token", nil) // same actor, different credential is independent
				require.Equal(t, "Bearer same-actor-token", next())
			}
			unblock()
			wg.Wait()
		})
	}
}

func TestDaemonMCPEventsLegacy(t *testing.T) {
	for _, identity := range []bool{false, true} {
		t.Run(strconv.FormatBool(identity), func(t *testing.T) {
			env, endpoint := mountedEventsFixture(t, identity, true)
			session := ""
			rpc := func(method string, params map[string]any) (int, string) {
				raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
				require.NoError(t, err)
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(raw))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer test-mcp-token")
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
				req.Header.Set("Mcp-Session-Id", session)
				resp, err := env.HTTP.Do(req)
				require.NoError(t, err)
				defer func() { _ = resp.Body.Close() }()
				if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
					session = id
				}
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				return resp.StatusCode, string(body)
			}
			status, body := rpc("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "legacy", "version": "1"}})
			require.Equal(t, http.StatusOK, status, body)
			require.NotEmpty(t, session)
			require.NotContains(t, body, `"events"`)
			require.NotContains(t, body, "io.modelcontextprotocol/events")
			for _, method := range []string{"events/list", "events/subscribe", "events/unsubscribe"} {
				status, body := rpc(method, eventsSubscribeParams("example-agent/worker", nil))
				require.Equal(t, http.StatusBadRequest, status, body)
				require.Contains(t, body, "not handled")
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, endpoint, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer test-mcp-token")
			req.Header.Set("Mcp-Session-Id", session)
			resp, err := env.HTTP.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
		})
	}
}

// Regression: a modern loader's selection survives request-scoped servers,
// while the same actor's other token and legacy sessions retain their catalogs.
func TestDaemonMCPIdentityModernLoadedSections(t *testing.T) {
	env, _ := mountedEventsFixture(t, true, false)
	endpoint := mountEventsEndpoint(t, env, true, nil, nil)
	for range 2 {
		_, loaded := mcpRPC(t, endpoint, "tools/call", map[string]any{"name": "kata.load_issue_mutation", "arguments": map[string]any{}})
		require.NotContains(t, loaded, "error", "%v", loaded)
		require.Equal(t, true, loaded["result"].(map[string]any)["structuredContent"].(map[string]any)["loaded"])
	}
	_, created := mcpRPC(t, endpoint, "tools/call", map[string]any{"name": "kata.create", "arguments": map[string]any{"project": "spoke-project", "title": "Modern fixture issue", "idempotency_key": "modern-section-regression"}})
	require.NotContains(t, created, "error", "%v", created)
	result := created["result"].(map[string]any)
	require.NotEqual(t, true, result["isError"], "%v", result)
	require.Equal(t, "example-agent", result["structuredContent"].(map[string]any)["issue"].(map[string]any)["author"])
	for _, caller := range []struct {
		bearer string
		loaded bool
	}{{"test-mcp-token", true}, {"same-actor-token", false}} {
		_, listed := mcpRPCWithBearer(t, endpoint, caller.bearer, "tools/list", map[string]any{})
		require.NotContains(t, listed, "error", "%v", listed)
		found := false
		for _, tool := range listed["result"].(map[string]any)["tools"].([]any) {
			found = found || tool.(map[string]any)["name"] == "kata.create"
		}
		require.Equal(t, caller.loaded, found, caller.bearer)
	}
	session, err := newMCPHTTPTestClient().Connect(t.Context(), newAuthenticatedMCPHTTPTestTransport(endpoint), &sdkmcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	require.NoError(t, err)
	defer func() { _ = session.Close() }()
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	for _, tool := range listed.Tools {
		require.NotEqual(t, "kata.create", tool.Name)
	}
}

func TestDaemonMCPEventsDisabled(t *testing.T) {
	for _, identity := range []bool{false, true} {
		t.Run(strconv.FormatBool(identity), func(t *testing.T) {
			_, endpoint := mountedEventsFixture(t, identity, false)
			status, listing := mcpRPC(t, endpoint, "events/list", map[string]any{})
			require.Equal(t, http.StatusOK, status)
			require.NotContains(t, listing, "error")
			result := listing["result"].(map[string]any)
			require.Equal(t, "complete", result["resultType"])
			require.Empty(t, result["events"])
			for _, method := range []string{"events/subscribe", "events/unsubscribe"} {
				status, wire := mcpRPC(t, endpoint, method, eventsSubscribeParams("example-agent/worker", nil))
				require.Equal(t, http.StatusOK, status)
				requireEventError(t, wire, mcpevents.CodeUnavailable)
				require.Equal(t, "events_not_configured", wire["error"].(map[string]any)["data"].(map[string]any)["reason"])
			}
		})
	}
}
