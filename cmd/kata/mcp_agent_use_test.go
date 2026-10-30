package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/hooks"
	mcpserver "go.kenn.io/kata/internal/mcp"
	"go.kenn.io/kata/internal/version"
	kataclient "go.kenn.io/kata/pkg/client"
)

func agentUseMCPSession(t *testing.T, apiClient *kataclient.Client) *sdkmcp.ClientSession {
	t.Helper()
	observe, stop := startMCPAgentUseReporter(t.Context(), apiClient, client.ResolvedDaemon{})
	t.Cleanup(stop)
	server, err := mcpserver.New(mcpserver.Options{
		Client: apiClient, ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test-version", ObserveToolCall: observe,
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
	t.Setenv("KATA_HOME", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		started := make(chan struct{}, 1)
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/ui/telemetry" {
				select {
				case started <- struct{}{}:
				default:
				}
				<-release
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/health" {
				_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.27.0"}`))
				return
			}
			_, _ = w.Write([]byte(`{"issues":[],"complete":true,"truncated":false}`))
		}))
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
		synctest.Wait()
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{}})
		require.NoError(t, err)
		require.False(t, result.IsError)
	})
}

func TestMCPAgentReporterBoundsQueueAndJoinsOnShutdown(t *testing.T) {
	for _, mode := range []string{"overflow", "shutdown", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
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
				observe, stop := startMCPAgentUseReporter(t.Context(), c, client.ResolvedDaemon{BaseURL: "http://daemon.example"})
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

func TestMCPAgentUseFullDay(t *testing.T) {
	for _, test := range []struct {
		name, receipt string
		stops         bool
	}{
		{"older daemon queued", "queued", false},
		{"full", "full", true},
		{"telemetry disabled", "disabled", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				setCLIUseClock(t, time.Date(2026, 10, 2, 23, 59, 0, 0, time.UTC))
				var calls atomic.Int32
				httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"` + test.receipt + `"}`))}, nil
				})}
				c, err := kataclient.NewWithHTTPClient("http://daemon.example", httpClient)
				require.NoError(t, err)
				observe, stop := startMCPAgentUseReporter(t.Context(), c, client.ResolvedDaemon{BaseURL: "http://daemon.example"})
				defer stop()
				for range 3 {
					observe()
				}
				synctest.Wait()
				want := int32(3)
				if test.stops {
					want = 1
				}
				require.Equal(t, want, calls.Load())
				stop()
				observe, stop = startMCPAgentUseReporter(t.Context(), c, client.ResolvedDaemon{BaseURL: "http://daemon.example"})
				defer stop()
				observe()
				synctest.Wait()
				if !test.stops {
					want++
				}
				require.Equal(t, want, calls.Load(), "a fresh reporter reuses the marker")
				if test.stops {
					setCLIUseClock(t, time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
					observe()
					synctest.Wait()
					require.Equal(t, want+1, calls.Load())
				}
			})
		})
	}
}

func TestMCPAgentUseSeesHookFullDayMarker(t *testing.T) {
	for _, receipt := range []string{"full", "disabled"} {
		t.Run(receipt, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			t.Setenv("KATA_AUTH_TOKEN", "")
			t.Setenv(hooks.HookVersionEnv, "1")
			var posts atomic.Int32
			server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/ui/telemetry", r.URL.Path)
				status := "queued"
				if posts.Add(1) == 2 {
					status = receipt
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"` + status + `"}`))
			}))
			server.Config.SetKeepAlivesEnabled(false)
			server.Start()
			synctest.Test(t, func(t *testing.T) {
				setCLIUseClock(t, time.Date(2026, 10, 2, 23, 59, 0, 0, time.UTC))
				c, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
				require.NoError(t, err)
				observe, stop := startMCPAgentUseReporter(t.Context(), c, client.ResolvedDaemon{BaseURL: server.URL})
				defer stop()
				observe()
				synctest.Wait()
				require.EqualValues(t, 1, posts.Load())

				reportCLIUseTo(t, server.URL)
				require.EqualValues(t, 2, posts.Load())
				observe()
				synctest.Wait()
				require.EqualValues(t, 2, posts.Load(), "the running reporter sees the hook's marker")
				require.NoError(t, os.Remove(client.TelemetryDayMarkerPath(client.ResolvedDaemon{BaseURL: server.URL}, "agent-full-")))
				if receipt == "disabled" {
					require.NoError(t, os.Remove(client.TelemetryDisabledMarkerPath(server.URL)))
				}
				observe()
				synctest.Wait()
				require.EqualValues(t, 2, posts.Load(), "persisted stop stays cached for the day")
				setCLIUseClock(t, time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
				observe()
				synctest.Wait()
				require.EqualValues(t, 3, posts.Load())
			})
		})
	}
}

func TestMCPCommandTelemetryAdmission(t *testing.T) {
	for _, scenario := range []string{"release", "KATA_TELEMETRY_ENABLED", "dev"} {
		t.Run(scenario, func(t *testing.T) {
			saved := version.Version
			t.Cleanup(func() { version.Version = saved })
			switch scenario {
			case "dev":
				version.Version = "dev"
			case "KATA_TELEMETRY_ENABLED":
				t.Setenv(scenario, "0")
			}
			capture := &cliUseTelemetry{}
			env := newCLIUseEnv(t, capture)
			t.Setenv("KATA_AUTHOR", "example-agent")
			target, err := url.Parse(env.URL)
			require.NoError(t, err)
			forward := &httputil.ReverseProxy{
				Rewrite:   func(request *httputil.ProxyRequest) { request.SetURL(target) },
				Transport: env.HTTP.Transport,
			}
			var posts, completed atomic.Int32
			forwarded := make(chan struct{}, 1)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/ui/telemetry" {
					posts.Add(1)
					select {
					case forwarded <- struct{}{}:
					default:
					}
				}
				forward.ServeHTTP(w, r)
				if r.URL.Path == "/api/v1/ui/telemetry" {
					completed.Add(1)
				}
			}))
			t.Cleanup(proxy.Close)
			endpoint := startMCPHTTPTestServer(t, proxy.URL)
			session, err := newMCPHTTPTestClient().Connect(t.Context(), newAuthenticatedMCPHTTPTestTransport(endpoint), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, session.Close()) }()
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.load_issue_discovery"})
			require.NoError(t, err)
			require.False(t, result.IsError)
			if scenario == "release" {
				//nolint:kennlint // sleeptest: Wait for the real daemon to finish responding to forwarded activity.
				require.Eventually(t, func() bool { return posts.Load() > 0 && posts.Load() == completed.Load() }, 5*time.Second, time.Millisecond)
				require.Positive(t, posts.Load(), "an eligible MCP command must forward activity")
			} else {
				// The command exposes no worker-drain signal, and stopping it can discard queued reports.
				// Observe twice the worker's one-second request budget, failing as soon as a report arrives.
				select {
				case <-forwarded:
					t.Fatal("an ineligible MCP command forwarded activity to an enabled daemon")
				case <-time.After(2 * time.Second):
				}
			}
		})
	}
}
