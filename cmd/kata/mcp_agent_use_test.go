package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	completed := make(chan struct{}, 1)
	httpClient := *env.HTTP
	transport := httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if r.URL.Path == "/api/v1/ui/telemetry" {
			completed <- struct{}{}
		}
		return response, err
	})
	c, err := kataclient.NewWithHTTPClient(env.URL, &httpClient)
	require.NoError(t, err)
	session := agentUseMCPSession(t, c)
	_, _ = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.load_issue_discovery", Arguments: map[string]any{}})
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("report did not complete")
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Equal(t, []string{"agent_active"}, capture.events)
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
	for _, mode := range []string{"overflow", "shutdown", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				started := make(chan struct{})
				var calls atomic.Int64
				httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
						close(started)
						select {
						case <-release:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}, nil
				})}
				c, err := kataclient.NewWithHTTPClient("http://daemon.example", httpClient)
				require.NoError(t, err)
				observe, stop := startMCPAgentUseReporter(t.Context(), c)
				defer stop()
				observe()
				<-started
				synctest.Wait()
				start := time.Now()
				queued := 1000
				if mode == "timeout" {
					queued = 1
				}
				for range queued {
					observe()
				}
				require.Zero(t, time.Since(start), "observations do not wait for delivery")
				if mode == "overflow" {
					close(release)
					synctest.Wait()
					require.EqualValues(t, 129, calls.Load())
				}
				if mode == "timeout" {
					time.Sleep(time.Second)
					synctest.Wait()
					require.EqualValues(t, 2, calls.Load())
				}
				start = time.Now()
				stop()
				require.Zero(t, time.Since(start), "shutdown cancels delivery and joins the worker")
				observe()
				synctest.Wait()
				if mode == "shutdown" {
					require.EqualValues(t, 1, calls.Load(), "queued observations are discarded on shutdown")
				}
			})
		})
	}
}
