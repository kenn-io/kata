package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	internalclient "go.kenn.io/kata/internal/client"
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

func TestMCPServeAllProjectsServesDaemonWideScope(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	var requests int
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
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

func TestMCPServeDefaultRequiresWorkspaceBinding(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	t.Chdir(t.TempDir())
	daemon := httptest.NewServer(http.HandlerFunc(http.NotFound))
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
