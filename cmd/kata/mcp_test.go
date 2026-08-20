package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	internalclient "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/version"
)

func TestMCPServeProjectModesRejectFlagConflicts(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "allowlist with project", args: []string{"--project", "spoke-project", "mcp", "serve", "--projects", "hub-project"}},
		{name: "allowlist with workspace", args: []string{"--workspace", t.TempDir(), "mcp", "serve", "--projects", "spoke-project"}},
		{name: "all projects with project", args: []string{"--project", "spoke-project", "mcp", "serve", "--all-projects"}},
		{name: "all projects with workspace", args: []string{"--workspace", t.TempDir(), "mcp", "serve", "--all-projects"}},
		{name: "all projects with allowlist", args: []string{"mcp", "serve", "--all-projects", "--projects", "spoke-project"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := newRootCmd()
			command.SetArgs(tt.args)
			err := command.Execute()
			require.ErrorContains(t, err, "cannot be combined")
		})
	}
}

func TestMCPServeTokenAdminRequiresAllProjects(t *testing.T) {
	for _, args := range [][]string{
		{"mcp", "serve", "--enable-token-admin"},
		{"--project", "spoke-project", "mcp", "serve", "--enable-token-admin"},
	} {
		command := newRootCmd()
		command.SetArgs(args)
		require.ErrorContains(t, command.Execute(), "--enable-token-admin requires --all-projects")
	}
}

// serveMCPTestHealth answers the startup API version handshake as a current
// daemon so tests can exercise the rest of the fake daemon.
func serveMCPTestHealth(writer http.ResponseWriter, request *http.Request) bool {
	if request.URL.Path != "/api/v1/health" {
		return false
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(writer, `{"ok":true,"api_schema_version":%q}`, daemon.APISchemaVersion)
	return true
}

func startMCPHTTPTestServer(t *testing.T, daemonURL string, extraArgs ...string) string {
	t.Helper()
	t.Setenv("KATA_MCP_TEST_TOKEN", "test-mcp-token")
	command := newRootCmd()
	stderrReader, stderrWriter := io.Pipe()
	command.SetOut(io.Discard)
	command.SetErr(stderrWriter)
	command.SetArgs(append([]string{
		"mcp", "serve", "--all-projects", "--http", "127.0.0.1:0",
		"--http-token-env", "KATA_MCP_TEST_TOKEN",
	}, extraArgs...))
	commandContext, cancel := context.WithCancel(context.WithValue(
		t.Context(), internalclient.BaseURLKey{}, daemonURL,
	))
	command.SetContext(commandContext)
	done := make(chan error, 1)
	go func() {
		done <- command.Execute()
		_ = stderrWriter.Close()
	}()

	firstLine := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderrReader)
		if scanner.Scan() {
			firstLine <- scanner.Text()
		}
		for scanner.Scan() {
		}
	}()

	const prefix = "kata mcp: listening on "
	var endpoint string
	select {
	case line := <-firstLine:
		if !strings.HasPrefix(line, prefix) {
			cancel()
			<-done
			t.Fatalf("unexpected MCP listener output %q", line)
		}
		endpoint = strings.TrimPrefix(line, prefix)
	case err := <-done:
		cancel()
		require.NoError(t, err, "MCP HTTP server exited before listening")
		t.Fatal("MCP HTTP server exited before reporting its endpoint")
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for MCP HTTP listener")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		// Let the server report its own bounded shutdown error instead of
		// racing a shorter test deadline under load.
		case <-time.After(mcpHTTPShutdownTimeout + 5*time.Second):
			t.Error("MCP HTTP server did not stop after context cancellation")
		}
		_ = stderrReader.Close()
	})
	return endpoint
}

func newMCPHTTPTestClient() *sdkmcp.Client {
	return sdkmcp.NewClient(&sdkmcp.Implementation{
		Name:    "kata-http-test",
		Version: "1.0.0",
	}, nil)
}

func newAuthenticatedMCPHTTPTestTransport(endpoint string) *sdkmcp.StreamableClientTransport {
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.Header.Set("Authorization", "Bearer test-mcp-token")
		return http.DefaultTransport.RoundTrip(clone)
	})}
	return &sdkmcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}
}

func TestMCPServeHTTPServesStreamableEndpointAndHealth(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(daemon.Close)

	endpoint := startMCPHTTPTestServer(t, daemon.URL)
	healthRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		strings.TrimSuffix(endpoint, "/mcp")+"/healthz", nil)
	require.NoError(t, err)
	healthResponse, err := http.DefaultClient.Do(healthRequest)
	require.NoError(t, err)
	t.Cleanup(func() { _ = healthResponse.Body.Close() })
	require.Equal(t, http.StatusOK, healthResponse.StatusCode)

	client := newMCPHTTPTestClient()
	connectContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	session, err := client.Connect(connectContext, newAuthenticatedMCPHTTPTestTransport(endpoint), nil)
	require.NoError(t, err)
	defer func() { _ = session.Close() }()
	tools, err := session.ListTools(connectContext, nil)
	require.NoError(t, err)
	require.NotEmpty(t, tools.Tools)
}

func TestMCPServeHTTPBearerProtectsMCPButNotHealth(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	t.Setenv("KATA_MCP_TEST_TOKEN", "test-mcp-token")
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(daemon.Close)

	endpoint := startMCPHTTPTestServer(t, daemon.URL)
	healthResponse, err := http.Get(strings.TrimSuffix(endpoint, "/mcp") + "/healthz") //nolint:noctx // bounded local test server
	require.NoError(t, err)
	require.NoError(t, healthResponse.Body.Close())
	require.Equal(t, http.StatusOK, healthResponse.StatusCode)

	connectContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = newMCPHTTPTestClient().Connect(connectContext, &sdkmcp.StreamableClientTransport{
		Endpoint:             endpoint,
		DisableStandaloneSSE: true,
	}, nil)
	require.Error(t, err, "MCP requests without the configured bearer must be rejected")

	session, err := newMCPHTTPTestClient().Connect(
		connectContext,
		newAuthenticatedMCPHTTPTestTransport(endpoint),
		nil,
	)
	require.NoError(t, err)
	require.NoError(t, session.Close())
}

func TestMCPServeHTTPCrossOriginBrowserMutationRejected(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(daemon.Close)

	endpoint := startMCPHTTPTestServer(t, daemon.URL)
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"browser","version":"1.0.0"}}}`)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, body)
	require.NoError(t, err)
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer test-mcp-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://attacker.example")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusForbidden, response.StatusCode)
}

func TestMCPServeHTTPLocalhostProtectionRejectsForeignHost(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(daemon.Close)

	endpoint := startMCPHTTPTestServer(t, daemon.URL)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	require.NoError(t, err)
	request.Host = "attacker.example"
	request.Header.Set("Authorization", "Bearer test-mcp-token")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusForbidden, response.StatusCode)
}

func TestMCPServeHTTPRejectsLoopbackHostThatDoesNotMatchConfiguredAuthority(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(daemon.Close)

	endpoint := startMCPHTTPTestServer(t, daemon.URL)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimSuffix(endpoint, "/mcp"), "http://"))
	require.NoError(t, err)
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"browser","version":"1.0.0"}}}`)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, body)
	require.NoError(t, err)
	request.Host = net.JoinHostPort("localhost", port)
	request.Header.Set("Origin", "http://"+request.Host)
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer test-mcp-token")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusForbidden, response.StatusCode)
}

func TestResolveMCPHTTPTokenLoopbackRequiresBearer(t *testing.T) {
	_, err := resolveMCPHTTPToken("127.0.0.1:8080", "", false)
	require.ErrorContains(t, err, "--http-token-env")
}

func TestMCPServeHTTPLoopbackRequiresBearer(t *testing.T) {
	command := newRootCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"mcp", "serve", "--all-projects", "--http", "127.0.0.1:0"})
	require.ErrorContains(t, command.Execute(), "--http-token-env")
}

func TestRunMCPHTTPServerWaitsForInflightHandlersDuringShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	server := &http.Server{
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			close(finished)
			writer.WriteHeader(http.StatusNoContent)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveContext, cancel := context.WithCancel(t.Context())
	serveDone := make(chan error, 1)
	go func() { serveDone <- runMCPHTTPServer(serveContext, server, listener) }()

	requestDone := make(chan error, 1)
	go func() {
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String(), nil)
		if requestErr != nil {
			requestDone <- requestErr
			return
		}
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			requestErr = response.Body.Close()
		}
		requestDone <- requestErr
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for HTTP handler to start")
	}
	cancel()
	select {
	case err := <-serveDone:
		t.Fatalf("server returned before its in-flight handler completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-serveDone)
	select {
	case <-finished:
	default:
		t.Fatal("server returned before handler cleanup completed")
	}
	require.NoError(t, <-requestDone)
}

func TestMCPServeHTTPNonLoopbackRequiresToken(t *testing.T) {
	command := newRootCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"mcp", "serve", "--all-projects", "--http", "0.0.0.0:0"})
	require.ErrorContains(t, command.Execute(), "--http-token-env")
}

func TestMCPServeHTTPNonLoopbackRequiresExplicitPrivateNetworkTrust(t *testing.T) {
	t.Setenv("KATA_MCP_TEST_TOKEN", "test-mcp-token")
	command := newRootCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{
		"mcp", "serve", "--all-projects", "--http", "0.0.0.0:0",
		"--http-token-env", "KATA_MCP_TEST_TOKEN",
	})
	require.ErrorContains(t, command.Execute(), "--trust-private-network")
}

func TestMCPServeHTTPTrustedPrivateNetworkRejectsPublicAndHostnameBinds(t *testing.T) {
	t.Setenv("KATA_MCP_TEST_TOKEN", "test-mcp-token")
	for _, address := range []string{"203.0.113.10:8080", "daemon.example:8080"} {
		t.Run(address, func(t *testing.T) {
			_, err := resolveMCPHTTPToken(address, "KATA_MCP_TEST_TOKEN", true)
			require.ErrorContains(t, err, "non-public")
		})
	}
}

func TestMCPServeHTTPTrustedPrivateNetworkAllowsPrivateAndWildcardBinds(t *testing.T) {
	t.Setenv("KATA_MCP_TEST_TOKEN", "test-mcp-token")
	for _, address := range []string{"10.0.0.5:8080", "100.64.0.5:8080", "0.0.0.0:8080", "[::]:8080"} {
		t.Run(address, func(t *testing.T) {
			token, err := resolveMCPHTTPToken(address, "KATA_MCP_TEST_TOKEN", true)
			require.NoError(t, err)
			require.Equal(t, "test-mcp-token", token)
		})
	}
}

func TestMCPServeHTTPTokenEnvMustBeSet(t *testing.T) {
	t.Setenv("KATA_MCP_MISSING_TOKEN", "")
	command := newRootCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{
		"mcp", "serve", "--all-projects", "--http", "127.0.0.1:0",
		"--http-token-env", "KATA_MCP_MISSING_TOKEN",
	})
	require.ErrorContains(t, command.Execute(), "KATA_MCP_MISSING_TOKEN")
}

func TestMCPServeAllProjectsServesDaemonWideScope(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	var requests int
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		requests++
		http.Error(writer, "unexpected project resolution", http.StatusInternalServerError)
	}))
	t.Cleanup(daemon.Close)

	command := newRootCmd()
	command.SetIn(bytes.NewReader(nil))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"mcp", "serve", "--all-projects"})
	command.SetContext(context.WithValue(t.Context(), internalclient.BaseURLKey{}, daemon.URL))

	require.NoError(t, command.Execute())
	require.Zero(t, requests, "daemon-wide startup must not resolve the current workspace")
}

func TestMCPServeRejectsDaemonBeforeRelationshipPinning(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	var otherRequests int
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/health" {
			_, _ = writer.Write([]byte(`{"ok":true,"api_schema_version":"0.10.0"}`))
			return
		}
		otherRequests++
		http.NotFound(writer, request)
	}))
	t.Cleanup(daemon.Close)

	command := newRootCmd()
	command.SetIn(bytes.NewReader(nil))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"mcp", "serve", "--all-projects"})
	command.SetContext(context.WithValue(t.Context(), internalclient.BaseURLKey{}, daemon.URL))

	err := command.Execute()
	require.ErrorContains(t, err, "requires daemon API 0.11.0 or newer")
	require.ErrorContains(t, err, "reports 0.10.0")
	require.Zero(t, otherRequests, "an incompatible daemon must be rejected before any MCP traffic")
}

func TestMCPServeDefaultRequiresWorkspaceBinding(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	t.Chdir(t.TempDir())
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(daemon.Close)

	command := newRootCmd()
	command.SetIn(bytes.NewReader(nil))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"mcp", "serve"})
	command.SetContext(context.WithValue(t.Context(), internalclient.BaseURLKey{}, daemon.URL))

	require.ErrorContains(t, command.Execute(), "--all-projects")
}

func TestParseMCPStorageTargets(t *testing.T) {
	targets, err := parseMCPStorageTargets([]string{"restore=restore.db", "archive=postgres://db.example/kata"})
	require.NoError(t, err)
	require.Equal(t, "restore.db", targets["restore"])
	require.Equal(t, "postgres://db.example/kata", targets["archive"])

	_, err = parseMCPStorageTargets([]string{"restore=one.db", "restore=two.db"})
	require.ErrorContains(t, err, "duplicate")
	_, err = parseMCPStorageTargets([]string{"postgres://user:hunter2@db.example/kata"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "hunter2", "malformed target values may carry credentials and must not be echoed")
}

const currentMCPProtocolVersion = "2026-07-28"

func TestMCPServeSyncOutlivesHandshakeTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("holds a sync response past the 10s SSE handshake timeout")
	}
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	workspace := t.TempDir()
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		switch request.URL.Path {
		case "/api/v1/projects/resolve":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"project":{"id":42,"name":"spoke-project"}}`))
		case "/api/v1/projects":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"projects":[{"id":42,"uid":"01HAAAAAAAAAAAAAAAAAAAAAAA","name":"spoke-project","metadata":{},"revision":1,"created_at":"2026-08-11T00:00:00Z"}]}`))
		case "/api/v1/projects/42/issue-sync/github/once":
			// A sync pass writes nothing, headers included, until it
			// completes; hold past the 10s SSE handshake timeout.
			select {
			case <-request.Context().Done():
				return
			case <-time.After(10500 * time.Millisecond):
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{
				"binding": {"id": 1, "project_id": 42, "provider": "github", "source_key": "github:node", "remote_id": "node", "display_name": "example/repo", "enabled": true, "interval_seconds": 900, "created_at": "2026-08-11T00:00:00Z", "updated_at": "2026-08-11T00:00:00Z"},
				"import": {"source": "github", "created": 0, "updated": 0, "unchanged": 0, "comments": 0, "links": 0, "errors": [], "items": []},
				"status": {"binding_id": 1, "project_id": 42, "provider": "github", "state": "idle", "enabled": true, "last_created": 0, "last_updated": 0, "last_unchanged": 0, "last_comments": 0}
			}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(daemon.Close)

	command := newRootCmd()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	command.SetIn(inputReader)
	command.SetOut(outputWriter)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--workspace", workspace, "mcp", "serve"})
	command.SetContext(context.WithValue(t.Context(), internalclient.BaseURLKey{}, daemon.URL))

	done := make(chan error, 1)
	go func() { done <- command.Execute() }()
	responses := bufio.NewReader(outputReader)
	send := func(id int, method string, params map[string]any) map[string]any {
		t.Helper()
		requestBytes, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		require.NoError(t, err)
		_, err = inputWriter.Write(append(requestBytes, '\n'))
		require.NoError(t, err)
		line, err := responses.ReadBytes('\n')
		require.NoError(t, err)
		var response map[string]any
		require.NoError(t, json.Unmarshal(line, &response))
		require.NotContains(t, response, "error", string(line))
		return response["result"].(map[string]any)
	}

	send(1, "server/discover", map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion":    currentMCPProtocolVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}})
	send(2, "tools/call", map[string]any{"name": "kata.load_sync", "arguments": map[string]any{}})
	result := send(3, "tools/call", map[string]any{"name": "kata.sync_once", "arguments": map[string]any{}})
	require.NotEqual(t, true, result["isError"],
		"a sync pass whose headers arrive after 10s must not be aborted by a response-header timeout: %v", result)
	structured := result["structuredContent"].(map[string]any)
	require.Equal(t, "idle", structured["status"].(map[string]any)["state"])

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestMCPServeEventWaitOutlivesDefaultClientTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("holds an event wait past the 5s default client timeout")
	}
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	workspace := t.TempDir()
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		switch request.URL.Path {
		case "/api/v1/projects/resolve":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"project":{"id":42,"name":"spoke-project"}}`))
		case "/api/v1/projects":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"projects":[{"id":42,"uid":"01HAAAAAAAAAAAAAAAAAAAAAAA","name":"spoke-project","metadata":{},"revision":1,"created_at":"2026-08-11T00:00:00Z"}]}`))
		case "/api/v1/events/stream":
			// Send SSE headers immediately, then hold the stream open with
			// no events past the 5s default client timeout.
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			<-request.Context().Done()
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(daemon.Close)

	command := newRootCmd()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	command.SetIn(inputReader)
	command.SetOut(outputWriter)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--workspace", workspace, "mcp", "serve"})
	command.SetContext(context.WithValue(t.Context(), internalclient.BaseURLKey{}, daemon.URL))

	done := make(chan error, 1)
	go func() { done <- command.Execute() }()
	responses := bufio.NewReader(outputReader)
	send := func(id int, method string, params map[string]any) map[string]any {
		t.Helper()
		requestBytes, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		require.NoError(t, err)
		_, err = inputWriter.Write(append(requestBytes, '\n'))
		require.NoError(t, err)
		line, err := responses.ReadBytes('\n')
		require.NoError(t, err)
		var response map[string]any
		require.NoError(t, json.Unmarshal(line, &response))
		require.NotContains(t, response, "error", string(line))
		return response["result"].(map[string]any)
	}

	send(1, "server/discover", map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion":    currentMCPProtocolVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}})
	send(2, "tools/call", map[string]any{"name": "kata.load_activity", "arguments": map[string]any{}})
	started := time.Now()
	result := send(3, "tools/call", map[string]any{
		"name":      "kata.events",
		"arguments": map[string]any{"mode": "wait", "wait_seconds": 6},
	})
	require.GreaterOrEqual(t, time.Since(started), 5500*time.Millisecond,
		"the wait must outlive the 5s default client timeout")
	require.NotEqual(t, true, result["isError"], "%v", result)
	structured := result["structuredContent"].(map[string]any)
	require.Equal(t, true, structured["timed_out"],
		"an event wait longer than the default client timeout must reach its own deadline")

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestMCPServeOrdinaryRequestsUseDefaultClientTimeout(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	t.Setenv("KATA_HTTP_TIMEOUT", "100ms")
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		if request.URL.Path != "/api/v1/instance" {
			http.NotFound(writer, request)
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-time.After(time.Second):
			http.Error(writer, "delayed daemon response", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(daemon.Close)

	command := newRootCmd()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	command.SetIn(inputReader)
	command.SetOut(outputWriter)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"mcp", "serve", "--all-projects"})
	commandContext, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	t.Cleanup(cancel)
	command.SetContext(context.WithValue(commandContext, internalclient.BaseURLKey{}, daemon.URL))

	done := make(chan error, 1)
	go func() { done <- command.Execute() }()
	responses := bufio.NewReader(outputReader)
	send := func(id int, method string, params map[string]any) map[string]any {
		t.Helper()
		requestBytes, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		require.NoError(t, err)
		_, err = inputWriter.Write(append(requestBytes, '\n'))
		require.NoError(t, err)
		line, err := responses.ReadBytes('\n')
		require.NoError(t, err)
		var response map[string]any
		require.NoError(t, json.Unmarshal(line, &response))
		require.NotContains(t, response, "error", string(line))
		return response["result"].(map[string]any)
	}

	send(1, "server/discover", map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion":    currentMCPProtocolVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}})
	send(2, "tools/call", map[string]any{"name": "kata.load_system", "arguments": map[string]any{}})
	started := time.Now()
	result := send(3, "tools/call", map[string]any{"name": "kata.system", "arguments": map[string]any{}})
	require.Less(t, time.Since(started), 500*time.Millisecond,
		"ordinary daemon requests must use the configured client timeout")
	require.Equal(t, true, result["isError"], "%v", result)

	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestMCPServeBindsProjectAndUsesStdoutOnlyForProtocol(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	workspace := t.TempDir()
	var resolved bool
	var cataloged bool
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/projects/resolve":
			resolved = true
			_, _ = writer.Write([]byte(`{"project":{"id":42,"name":"spoke-project"}}`))
		case "/api/v1/projects":
			cataloged = true
			_, _ = writer.Write([]byte(`{"projects":[{"id":42,"uid":"01HAAAAAAAAAAAAAAAAAAAAAAA","name":"spoke-project","metadata":{},"revision":1,"created_at":"2026-08-11T00:00:00Z"}]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(daemon.Close)

	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "server/discover",
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    currentMCPProtocolVersion,
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	}
	requestBytes, err := json.Marshal(request)
	require.NoError(t, err)

	command := newRootCmd()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	var stderr bytes.Buffer
	command.SetIn(inputReader)
	command.SetOut(outputWriter)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--workspace", workspace, "mcp", "serve"})
	command.SetContext(context.WithValue(t.Context(), internalclient.BaseURLKey{}, daemon.URL))

	done := make(chan error, 1)
	go func() { done <- command.Execute() }()
	_, err = inputWriter.Write(append(requestBytes, '\n'))
	require.NoError(t, err)
	protocolLine, err := bufio.NewReader(outputReader).ReadBytes('\n')
	require.NoError(t, err)
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
	rest, err := io.ReadAll(outputReader)
	require.NoError(t, err)
	require.True(t, resolved)
	require.True(t, cataloged)
	require.Empty(t, stderr.String())
	require.Empty(t, rest, "stdout must contain protocol messages only")
	var response struct {
		Result struct {
			SupportedVersions []string `json:"supportedVersions"`
			Meta              struct {
				ServerInfo struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"io.modelcontextprotocol/serverInfo"`
			} `json:"_meta"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(protocolLine, &response))
	require.NotEmpty(t, response.Result.SupportedVersions)
	require.Contains(t, response.Result.SupportedVersions, currentMCPProtocolVersion)
	require.Equal(t, "kata", response.Result.Meta.ServerInfo.Name)
	require.Equal(t, version.Version, response.Result.Meta.ServerInfo.Version)
}
