package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/telemetry"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestDaemonMCPUnavailableHandler(t *testing.T) {
	var logs bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(original) })
	handler := daemonMCPUnavailableHandler(errors.New(`project "missing" was not found`))
	buildLog := logs.String()
	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(buildLog), &entry))
	require.Equal(t, "ERROR", entry["level"])
	require.Equal(t, "daemon MCP endpoint disabled", entry["msg"])
	require.Equal(t, `project "missing" was not found`, entry["err"])
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/mcp", nil))
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.Equal(t, "application/json", response.Header().Get("Content-Type"))
		require.JSONEq(t, `{"error":"kata MCP endpoint unavailable; see daemon log"}`, response.Body.String())
		require.NotContains(t, response.Body.String(), "missing")
	}
	require.Equal(t, buildLog, logs.String(), "requests must not repeat the startup log")
}

// The daemon mount uses its own MCP bearer and calls its authenticated REST API.
func TestDaemonServesMCPWithSeparateBearer(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	httpClient := &http.Client{Transport: transport}
	for _, tt := range []struct {
		name, listen, web, host, contentType string
		startup                              string
		unavailable, checkMountPolicy        bool
	}{
		{name: "shared TCP", listen: "127.0.0.1:0", web: "public_origin = 'https://daemon.example'\n", host: "daemon.example", contentType: "application/json", checkMountPolicy: true},
		{name: "mixed-case JSON", listen: "127.0.0.1:0", web: "public_origin = 'https://daemon.example'\n", host: "daemon.example", contentType: "Application/JSON"},
		{name: "Unix with web listener", web: "listen = '127.0.0.1:0'\n", contentType: "application/json"},
		{name: "startup sections", listen: "127.0.0.1:0", contentType: "application/json", startup: `startup_sections = [" issue_mutation "]`},
		{name: "unknown startup section", listen: "127.0.0.1:0", startup: `startup_sections = ["missing"]`, unavailable: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.listen == "" && runtime.GOOS == "windows" {
				t.Skip("Unix sockets are not supported on Windows")
			}
			resetFlags(t)
			home := deploymentHome(t)
			t.Setenv(daemonruntime.AutoStartMarkerEnv, "1")
			t.Setenv("KATA_AUTH_TOKEN", "example-daemon-token")
			t.Setenv("KATA_MCP_TEST_TOKEN", "test-mcp-token")
			t.Setenv("KATA_MCP_STARTUP_SECTIONS", "")
			capture := &cliUseTelemetry{}
			orig := newTelemetryReporter
			newTelemetryReporter = func(telemetry.Options) telemetry.Client { return capture }
			t.Cleanup(func() { newTelemetryReporter = orig })
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[mcp]
enabled = true
actor = " gateway-agent "
all_projects = true
token_env = "KATA_MCP_TEST_TOKEN"
`+tt.startup+`
[web]
`+tt.web), 0o600))
			store := openKataTestDB(t, filepath.Join(home, "kata.db"))
			_, err := store.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			require.NoError(t, store.Close())

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- runDaemonWithListen(ctx, tt.listen, false, false) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Errorf("daemon did not stop cleanly: %v", err)
					}
				// HTTP shutdown gives unused new connections over five seconds to idle.
				// Match the full-daemon federation ingress test's cleanup budget.
				case <-time.After(15 * time.Second):
					t.Error("daemon did not stop after context cancellation")
				}
			})
			ns, err := daemonruntime.NewNamespace()
			require.NoError(t, err)
			runtimePath, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Path(os.Getpid())
			require.NoError(t, err)
			var record kitdaemon.RuntimeRecord
			var startupErr error
			var exited bool
			require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: daemon startup publishes readiness only through its runtime record file
				select {
				case err := <-done:
					done <- err
					startupErr, exited = err, true
					return true
				default:
				}
				body, readErr := os.ReadFile(runtimePath) //nolint:gosec // test-owned runtime path
				return readErr == nil && json.Unmarshal(body, &record) == nil
			}, 5*time.Second, 20*time.Millisecond)
			require.False(t, exited, "daemon exited during startup: %v", startupErr)
			wantMCP := "serving"
			if tt.unavailable {
				wantMCP = "unavailable"
			}
			require.Equal(t, wantMCP, record.Metadata["mcp"])
			endpoint := "http://" + record.Address + "/mcp"
			if tt.listen == "" {
				require.Equal(t, kitdaemon.NetworkUnix, record.Network)
				endpoint = record.Metadata["web_origin"] + "/mcp"
			}
			callCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			defer stop()
			if tt.unavailable {
				request, err := http.NewRequestWithContext(callCtx, http.MethodGet, "http://"+record.Address+"/api/v1/ping", nil)
				require.NoError(t, err)
				response, err := httpClient.Do(request)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, http.StatusOK, response.StatusCode)

				request, err = http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, nil)
				require.NoError(t, err)
				response, err = httpClient.Do(request)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
			} else {
				if tt.checkMountPolicy {
					request, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, nil)
					require.NoError(t, err)
					request.Host = tt.host
					request.Header.Set("Authorization", "Bearer example-daemon-token")
					response, err := httpClient.Do(request)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					require.Equal(t, http.StatusUnauthorized, response.StatusCode)
				}
				mcpTransport := newAuthenticatedMCPHTTPTestTransport(endpoint)
				mcpTransport.HTTPClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
					clone := request.Clone(request.Context())
					clone.Host = tt.host
					clone.Header.Set("Authorization", "Bearer test-mcp-token")
					if clone.Method == http.MethodPost {
						clone.Header.Set("Content-Type", tt.contentType)
					}
					return transport.RoundTrip(clone)
				})
				session, err := newMCPHTTPTestClient().Connect(callCtx, mcpTransport, nil)
				require.NoError(t, err)
				defer func() { _ = session.Close() }()
				if tt.checkMountPolicy {
					loaded, err := session.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.load_tokens"})
					require.NoError(t, err)
					require.False(t, loaded.IsError)
					section := loaded.StructuredContent.(map[string]any)
					require.Equal(t, false, section["available"])
					require.Equal(t, false, section["loaded"])
				}
				tools, err := session.ListTools(callCtx, nil)
				require.NoError(t, err)
				if tt.startup != "" {
					require.True(t, slices.ContainsFunc(tools.Tools, func(tool *sdkmcp.Tool) bool { return tool.Name == "kata.create" }))
				}
				if tt.checkMountPolicy {
					loaded, err := session.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.load_issue_mutation"})
					require.NoError(t, err)
					require.False(t, loaded.IsError)
					created, err := session.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.create", Arguments: map[string]any{
						"project": "example-project", "title": "Gateway issue", "idempotency_key": "gateway-issue",
					}})
					require.NoError(t, err)
					require.False(t, created.IsError, "%v", created.Content)
					issue := created.StructuredContent.(map[string]any)["issue"].(map[string]any)
					require.Equal(t, "gateway-agent", issue["author"])
					//nolint:kennlint // sleeptest: Tool-use reporting finishes asynchronously through the daemon REST API.
					require.Eventually(t, func() bool {
						capture.mu.Lock()
						defer capture.mu.Unlock()
						return slices.Contains(capture.events, "agent_active")
					}, 5*time.Second, time.Millisecond)
				}
			}
		})
	}
}

func TestDaemonMCPSelfCallsBypassHTTPProxy(t *testing.T) {
	for _, tt := range []struct {
		name string
		auth config.AuthConfig
	}{
		{"bearer", config.AuthConfig{Token: "daemon-token", TrustPrivateNetwork: true}},
		{"tokenless private network", config.AuthConfig{AllowUnauthenticatedPrivateNetworkWrites: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := deploymentHome(t)
			store := openKataTestDB(t, filepath.Join(home, "kata.db"))
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
			authorization := ""
			if tt.auth.Token != "" {
				authorization = "Bearer " + tt.auth.Token
			}
			var proxyCalls, daemonCalls atomic.Int64
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				proxyCalls.Add(1)
				http.Error(w, "self-call reached proxy", http.StatusBadGateway)
			}))
			defer proxy.Close()
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				daemonCalls.Add(1)
				if r.URL.Path != "/api/v1/projects" || r.Header.Get("Authorization") != authorization {
					http.Error(w, "unexpected self-call", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"projects":[]}`))
			}))
			defer backend.Close()
			backendURL, err := url.Parse(backend.URL)
			require.NoError(t, err)
			backendAddress := backendURL.Host
			backendURL.Host = "100.64.0.5:7777"
			proxyURL, err := url.Parse(proxy.URL)
			require.NoError(t, err)
			base := http.DefaultTransport.(*http.Transport).Clone()
			defer base.CloseIdleConnections()
			base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if address == backendURL.Host {
					address = backendAddress
				}
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			base.Proxy = func(r *http.Request) (*url.URL, error) {
				if r.URL.Host == backendURL.Host {
					return proxyURL, nil
				}
				return nil, nil
			}
			original := http.DefaultTransport
			http.DefaultTransport = base
			t.Cleanup(func() { http.DefaultTransport = original })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			handler, err := newDaemonMCPHandler(ctx, store, config.MCPConfig{Actor: "gateway-agent", AllProjects: true}, "test-mcp-token",
				kitdaemon.Endpoint{Network: "tcp", Address: backendURL.Host},
				tt.auth)
			require.NoError(t, err)
			gateway := httptest.NewServer(handler)
			defer gateway.Close()
			session, err := newMCPHTTPTestClient().Connect(ctx, newAuthenticatedMCPHTTPTestTransport(gateway.URL+"/mcp"), nil)
			require.NoError(t, err)
			defer func() { _ = session.Close() }()
			loaded, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.load_projects"})
			require.NoError(t, err)
			require.False(t, loaded.IsError, "%v", loaded.Content)
			before := daemonCalls.Load()
			result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.projects", Arguments: map[string]any{}})
			require.NoError(t, err)
			require.Zero(t, proxyCalls.Load(), "self-call reached proxy")
			require.False(t, result.IsError, "%v", result.Content)
			require.Greater(t, daemonCalls.Load(), before)
			require.Empty(t, result.StructuredContent.(map[string]any)["projects"])
		})
	}
}

func TestDaemonMCPProjectScope(t *testing.T) {
	for _, tt := range []struct {
		name, projects, wantError string
		wantProjects              []string
		archiveAlpha              bool
	}{
		{name: "all projects", projects: "all", wantProjects: []string{"alpha", "beta"}},
		{name: "trimmed", projects: `[" alpha "]`, wantProjects: []string{"alpha"}},
		{name: "archived", projects: `["alpha"]`, archiveAlpha: true, wantError: `[mcp] projects: MCP project "alpha" was not found`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := deploymentHome(t)
			contents := "[mcp]\nactor = 'gateway-agent'\n"
			if tt.projects == "all" {
				contents += "all_projects = true\n"
			} else {
				contents += "projects = " + tt.projects + "\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(contents), 0o600))
			cfg, err := config.ReadDaemonConfig()
			require.NoError(t, err)
			store := openKataTestDB(t, filepath.Join(home, "kata.db"))
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			alpha, err := store.CreateProject(t.Context(), "alpha")
			require.NoError(t, err)
			_, err = store.CreateProject(t.Context(), "beta")
			require.NoError(t, err)
			if tt.archiveAlpha {
				_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: alpha.ID, Actor: "gateway-agent"})
				require.NoError(t, err)
			}
			backend := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "daemon-token"}})
			t.Cleanup(func() { require.NoError(t, backend.Close()) })
			rest := httptest.NewServer(backend.Handler())
			t.Cleanup(rest.Close)
			address, err := url.Parse(rest.URL)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			handler, err := newDaemonMCPHandler(ctx, store, cfg.MCP, "test-mcp-token", kitdaemon.Endpoint{Network: "tcp", Address: address.Host}, config.AuthConfig{Token: "daemon-token"})
			if tt.wantError != "" {
				require.EqualError(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			gateway := httptest.NewServer(handler)
			t.Cleanup(gateway.Close)
			session, err := newMCPHTTPTestClient().Connect(ctx, newAuthenticatedMCPHTTPTestTransport(gateway.URL+"/mcp"), nil)
			require.NoError(t, err)
			defer func() { _ = session.Close() }()
			loaded, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.load_projects"})
			require.NoError(t, err)
			require.False(t, loaded.IsError, "%v", loaded.Content)
			result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.projects"})
			require.NoError(t, err)
			require.False(t, result.IsError, "%v", result.Content)
			var names []string
			for _, project := range result.StructuredContent.(map[string]any)["projects"].([]any) {
				names = append(names, project.(map[string]any)["name"].(string))
			}
			require.ElementsMatch(t, tt.wantProjects, names)
		})
	}
}

func TestDaemonMCPPreflightRejectsInvalidAuthority(t *testing.T) {
	for _, tt := range []struct{ name, listen, config, want string }{
		{"identity mode", "127.0.0.1:7777", "[auth]\nrequire_token_identity = true\n[mcp]\nenabled = true\n", `[mcp] enabled cannot be combined with [auth] require_token_identity: the daemon MCP endpoint calls REST with the bootstrap token, which cannot perform attributed writes; run "kata mcp serve --http" with a user token instead`},
		{"proxy actor", "127.0.0.1:7777", "[auth.proxy]\ntrusted_actor_header = 'X-Kata-Actor'\ntrusted_proxy_listeners = ['127.0.0.1:7777']\n[mcp]\nenabled = true\nactor = 'gateway-agent'\ntoken_env = 'KATA_MCP_TEST_TOKEN'\n", `[mcp] enabled cannot be combined with [auth.proxy] trusted_actor_header: the daemon MCP endpoint's REST calls carry no proxy actor header; run "kata mcp serve --http" with REST access through a listener outside trusted_proxy_listeners or through the proxy that sets the actor header`},
		{"proxy actor on expanded IPv6", "[0:0:0:0:0:0:0:1]:7777", "[auth.proxy]\ntrusted_actor_header = 'X-Kata-Actor'\ntrusted_proxy_listeners = ['[::1]:7777']\n[mcp]\nenabled = true\nactor = 'gateway-agent'\ntoken_env = 'KATA_MCP_TEST_TOKEN'\n", `[mcp] enabled cannot be combined with [auth.proxy] trusted_actor_header: the daemon MCP endpoint's REST calls carry no proxy actor header; run "kata mcp serve --http" with REST access through a listener outside trusted_proxy_listeners or through the proxy that sets the actor header`},
		{"proxy actor on another listener", "127.0.0.1:7777", "[auth.proxy]\ntrusted_actor_header = 'X-Kata-Actor'\ntrusted_proxy_listeners = ['127.0.0.1:8888']\n[mcp]\nenabled = true\nactor = 'gateway-agent'\nall_projects = true\ntoken_env = 'KATA_MCP_TEST_TOKEN'\n", ""},
		{"no project scope", "127.0.0.1:7777", "[mcp]\nenabled = true\nactor = 'gateway-agent'\ntoken_env = 'KATA_MCP_TEST_TOKEN'\n", "[mcp] enabled requires projects or all_projects = true"},
		{"empty project list", "127.0.0.1:7777", "[mcp]\nenabled = true\nactor = 'gateway-agent'\nprojects = []\ntoken_env = 'KATA_MCP_TEST_TOKEN'\n", "[mcp] enabled requires projects or all_projects = true"},
		{"projects and all projects", "127.0.0.1:7777", "[mcp]\nenabled = true\nactor = 'gateway-agent'\nprojects = ['alpha']\nall_projects = true\ntoken_env = 'KATA_MCP_TEST_TOKEN'\n", "[mcp] projects and all_projects cannot both be set"},
		{"no token source", "127.0.0.1:7777", "[mcp]\nenabled = true\n", "[mcp] enabled requires token_file or token_env"},
		{"blank configured actor", "127.0.0.1:7777", "[mcp]\nenabled = true\nactor = '  '\ntoken_env = 'KATA_MCP_TEST_TOKEN'\n", "kata actor is required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags(t)
			home := deploymentHome(t)
			t.Setenv("KATA_MCP_TEST_TOKEN", "test-mcp-token")
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tt.config), 0o600))
			_, err := preflightDaemonStartup(t.Context(), tt.listen, false, false)
			if tt.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.want)
			}
		})
	}
}

// A "tcp" wildcard listener is dual-stack, but IPv6-disabled containers have no ::1.
func TestDaemonMCPSelfEndpointUsesIPv4LoopbackForWildcards(t *testing.T) {
	for listen, want := range map[string]string{
		"0.0.0.0:7777":           "127.0.0.1:7777",
		":7777":                  "127.0.0.1:7777",
		"[::]:7777":              "127.0.0.1:7777",
		"[0:0:0:0:0:0:0:0]:7777": "127.0.0.1:7777",
		"[::1]:7777":             "[::1]:7777",
		"100.64.0.5:7777":        "100.64.0.5:7777",
	} {
		self, err := daemonMCPSelfEndpoint(kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: listen})
		require.NoError(t, err)
		require.Equal(t, want, self.Address, listen)
	}
}
