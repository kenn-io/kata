package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"go.kenn.io/kata/internal/telemetry"
	kitdaemon "go.kenn.io/kit/daemon"
)

// The daemon mount uses its own MCP bearer and calls its authenticated REST API.
func TestDaemonServesMCPWithSeparateBearer(t *testing.T) {
	for _, tt := range []struct{ name, listen, web, host string }{
		{"shared TCP", "127.0.0.1:0", "public_origin = 'https://daemon.example'\n", "daemon.example"},
		{"expanded IPv6", "[0:0:0:0:0:0:0:1]:0", "public_origin = 'https://daemon.example'\n", "daemon.example"},
		{"Unix with web listener", "", "listen = '127.0.0.1:0'\n", ""},
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
			capture := &cliUseTelemetry{}
			orig := newTelemetryReporter
			newTelemetryReporter = func(telemetry.Options) telemetry.Client { return capture }
			t.Cleanup(func() { newTelemetryReporter = orig })
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[mcp]
enabled = true
actor = " gateway-agent "
token_env = "KATA_MCP_TEST_TOKEN"
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
				case <-time.After(3 * time.Second):
					t.Error("daemon did not stop after context cancellation")
				}
			})
			ns, err := daemonruntime.NewNamespace()
			require.NoError(t, err)
			runtimePath, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Path(os.Getpid())
			require.NoError(t, err)
			var record kitdaemon.RuntimeRecord
			require.Eventually(t, func() bool { //nolint:kennlint // daemon startup publishes readiness only through its runtime record file
				select {
				case err := <-done:
					t.Fatalf("daemon exited during startup: %v", err)
				default:
				}
				body, readErr := os.ReadFile(runtimePath) //nolint:gosec // test-owned runtime path
				return readErr == nil && json.Unmarshal(body, &record) == nil
			}, 5*time.Second, 20*time.Millisecond)
			endpoint := "http://" + record.Address + "/mcp"
			if tt.listen == "" {
				require.Equal(t, kitdaemon.NetworkUnix, record.Network)
				endpoint = record.Metadata["web_origin"] + "/mcp"
			}
			httpClient := &http.Client{Timeout: 2 * time.Second}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer example-daemon-token")
			response, err := httpClient.Do(request)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusUnauthorized, response.StatusCode)
			callCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			defer stop()
			session, err := newMCPHTTPTestClient().Connect(callCtx, newAuthenticatedMCPHTTPTestTransport(endpoint, tt.host), nil)
			require.NoError(t, err)
			defer func() { _ = session.Close() }()
			// Load the issue mutation tools before calling kata.create.
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
			//nolint:kennlint // Tool-use reporting finishes asynchronously through the daemon REST API.
			require.Eventually(t, func() bool {
				capture.mu.Lock()
				defer capture.mu.Unlock()
				return slices.Contains(capture.events, "agent_active")
			}, 5*time.Second, time.Millisecond)
		})
	}
}

func TestDaemonMCPSelfCallsBypassHTTPProxy(t *testing.T) {
	for _, tt := range []struct {
		name             string
		auth             config.AuthConfig
		insecureReadonly bool
	}{
		{"bearer", config.AuthConfig{Token: "daemon-token", TrustPrivateNetwork: true}, false},
		{"tokenless private network", config.AuthConfig{AllowUnauthenticatedPrivateNetworkWrites: true}, false},
		{"read-only private network", config.AuthConfig{}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			deploymentHome(t)
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
			require.NoError(t, daemon.CheckAuthStartup(backendURL.Host, tt.auth, tt.insecureReadonly))
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
			handler, err := newDaemonMCPHandler(ctx, config.MCPConfig{Actor: "gateway-agent"}, "test-mcp-token",
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

func TestDaemonMCPPreflightRejectsInvalidAuthority(t *testing.T) {
	for _, tt := range []struct{ name, config, want string }{
		{"identity mode", "[auth]\nrequire_token_identity = true\n[mcp]\nenabled = true\n", `[mcp] enabled cannot be combined with [auth] require_token_identity: the daemon MCP endpoint calls REST with the bootstrap token, which cannot perform attributed writes; run "kata mcp serve --http" with a user token instead`},
		{"proxy actor", "[auth.proxy]\ntrusted_actor_header = 'X-Kata-Actor'\ntrusted_proxy_listeners = ['127.0.0.1:7777']\n[mcp]\nenabled = true\nactor = 'gateway-agent'\ntoken_env = 'KATA_MCP_TEST_TOKEN'\n", `[mcp] enabled cannot be combined with [auth.proxy] trusted_actor_header: the daemon MCP endpoint's REST calls carry no proxy actor header; run "kata mcp serve --http" instead`},
		{"proxy actor on another listener", "[auth.proxy]\ntrusted_actor_header = 'X-Kata-Actor'\ntrusted_proxy_listeners = ['127.0.0.1:8888']\n[mcp]\nenabled = true\nactor = 'gateway-agent'\ntoken_env = 'KATA_MCP_TEST_TOKEN'\n", ""},
		{"no token source", "[mcp]\nenabled = true\n", "[mcp] enabled requires token_file or token_env"},
		{"blank configured actor", "[mcp]\nenabled = true\nactor = '  '\ntoken_env = 'KATA_MCP_TEST_TOKEN'\n", "kata actor is required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags(t)
			home := deploymentHome(t)
			t.Setenv("KATA_MCP_TEST_TOKEN", "test-mcp-token")
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tt.config), 0o600))
			_, err := preflightDaemonStartup(t.Context(), "127.0.0.1:7777", false, false)
			if tt.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.want)
			}
		})
	}
}
