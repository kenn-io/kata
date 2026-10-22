package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/telemetry"
	kitdaemon "go.kenn.io/kit/daemon"
)

// The daemon mount uses its own MCP bearer and calls its authenticated REST API.
func TestDaemonServesMCPWithSeparateBearer(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	t.Setenv("PORT", "")
	t.Setenv(daemonruntime.AutoStartMarkerEnv, "1")
	t.Setenv("KATA_AUTH_TOKEN", "example-daemon-token")
	t.Setenv("KATA_MCP_TEST_TOKEN", "test-mcp-token")
	orig := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client { return &fakeTelemetryReporter{} }
	t.Cleanup(func() { newTelemetryReporter = orig })
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[mcp]
enabled = true
actor = "gateway-agent"
token_env = "KATA_MCP_TEST_TOKEN"
`), 0o600))
	store := openKataTestDB(t, filepath.Join(home, "kata.db"))
	_, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	require.NoError(t, store.Close())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runDaemonWithListen(ctx, "127.0.0.1:0", false, false) }()
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
	var record daemonRuntimeRecordJSON
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
	httpClient := &http.Client{Timeout: 2 * time.Second}
	for _, token := range []string{"", "example-daemon-token"} {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
		require.NoError(t, err)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := httpClient.Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	}
	callCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	session, err := newMCPHTTPTestClient().Connect(callCtx, newAuthenticatedMCPHTTPTestTransport(endpoint), nil)
	require.NoError(t, err)
	defer func() { _ = session.Close() }()
	loaded, err := session.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.load_projects"})
	require.NoError(t, err)
	require.False(t, loaded.IsError)
	projects, err := session.CallTool(callCtx, &sdkmcp.CallToolParams{Name: "kata.projects"})
	require.NoError(t, err)
	require.False(t, projects.IsError)
	body, err := json.Marshal(projects.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(body), `"name":"example-project"`)
}

func TestDaemonMCPRequiresTokenSource(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	t.Setenv("PORT", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[mcp]\nenabled = true\n"), 0o600))
	_, err := preflightDaemonStartup(t.Context(), "127.0.0.1:0", false, false)
	require.ErrorContains(t, err, "[mcp] enabled requires token_file or token_env")
}
