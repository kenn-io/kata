package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	mcpserver "go.kenn.io/kata/internal/mcp"
	kataclient "go.kenn.io/kata/pkg/client"
)

func agentUseMCPSession(t *testing.T, client *kataclient.Client) *sdkmcp.ClientSession {
	t.Helper()
	observe, stop := startMCPAgentUseReporter(t.Context(), client)
	t.Cleanup(stop)
	server, err := mcpserver.New(mcpserver.Options{
		Client: client, ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test-version", ObserveToolCall: observe,
	})
	require.NoError(t, err)
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := newMCPHTTPTestClient().Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestMCPCallsReportDailyAgentActivity(t *testing.T) {
	capture := &cliUseTelemetry{}
	env := newCLIUseEnv(t, capture)
	c, err := kataclient.NewWithHTTPClient(env.URL, env.HTTP)
	require.NoError(t, err)
	session := agentUseMCPSession(t, c)
	_, err = session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Empty(t, capture.recorded())
	for call := 1; call <= 11; call++ {
		name := "kata.missing"
		if call == 1 {
			name = "kata.load_issue_discovery"
		}
		_, _ = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		require.Eventually(t, func() bool {
			return agentUseMarkerCount(env.Home) == call
		}, time.Second, 5*time.Millisecond)
		if call <= 10 {
			require.Equal(t, []map[string]any{{"call_count_bucket": "1-10"}}, capture.recorded())
		}
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Equal(t, []string{"agent_active", "agent_call_count"}, capture.events)
	require.Equal(t, []map[string]any{{"call_count_bucket": "1-10"}, {"call_count_bucket": "11-100"}}, capture.captured)
}

func TestMCPStalledTelemetryPreservesTypedCallDeadline(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ui/telemetry" {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[]}`))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	c, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	session := agentUseMCPSession(t, c)
	_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.load_issue_discovery"})
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("report did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError)
}

func TestMCPAgentReporterBoundsQueueAndJoinsOnShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "overflow", true: "shutdown"}[shutdown], func(t *testing.T) {
			release := make(chan struct{})
			started := make(chan struct{}, 1)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					started <- struct{}{}
					<-release
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(server.Close)
			if shutdown {
				t.Cleanup(func() { close(release) })
			}
			c, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			observe, stop := startMCPAgentUseReporter(t.Context(), c)
			t.Cleanup(stop)
			observe()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("report did not start")
			}
			start := time.Now()
			for range 1000 {
				observe()
			}
			require.Less(t, time.Since(start), 100*time.Millisecond)
			if !shutdown {
				close(release)
				require.Eventually(t, func() bool { return calls.Load() >= 129 }, time.Second, 5*time.Millisecond)
				require.Never(t, func() bool { return calls.Load() > 129 }, 100*time.Millisecond, 5*time.Millisecond)
			}
			start = time.Now()
			stop()
			require.Less(t, time.Since(start), time.Second)
			observe()
			if shutdown {
				require.EqualValues(t, 1, calls.Load(), "queued observations are discarded on shutdown")
			}
		})
	}
}

func TestMCPAgentReporterTimesOutAndContinues(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-release
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	c, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	observe, stop := startMCPAgentUseReporter(t.Context(), c)
	defer stop()
	observe()
	observe()
	require.Eventually(t, func() bool { return calls.Load() == 2 }, 2*time.Second, 10*time.Millisecond)
}

func agentUseMarkerCount(home string) int {
	paths, _ := filepath.Glob(filepath.Join(home, "telemetry", "agent-use-*.json"))
	if len(paths) != 1 {
		return 0
	}
	data, _ := os.ReadFile(paths[0])
	var state struct{ Count int }
	_ = json.Unmarshal(data, &state)
	return state.Count
}
