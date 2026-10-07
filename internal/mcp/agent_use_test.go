package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitestore"
	kataclient "go.kenn.io/kata/pkg/client"
)

type agentUseReporter struct {
	mu     sync.Mutex
	events []string
}

func (*agentUseReporter) Enabled() bool            { return true }
func (*agentUseReporter) EventAllowed(string) bool { return true }
func (r *agentUseReporter) Capture(event string, _ map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func TestMCPCallsReportDailyAgentActivity(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	r := &agentUseReporter{}
	d := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now(), Telemetry: r})
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	httpServer := httptest.NewServer(d.Handler())
	t.Cleanup(httpServer.Close)
	c, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	session := connectRawTestServerWithOptions(t, Options{
		Client: c, ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test-version",
	})
	_, err = session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	r.mu.Lock()
	require.Empty(t, r.events)
	r.mu.Unlock()
	for _, name := range []string{"kata.load_issue_discovery", "kata.missing"} {
		_, _ = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{}})
	}
	r.mu.Lock()
	require.Equal(t, []string{"agent_active"}, r.events)
	r.mu.Unlock()
	for range 9 {
		_, _ = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.missing", Arguments: map[string]any{}})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Equal(t, []string{"agent_active", "agent_call_count"}, r.events, "tool errors count; human activity stays separate")
}

func TestMCPAgentReportFailurePreservesToolResult(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "timeout"}[slow], func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if slow {
					<-release
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() { close(release) })
			client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			toolError := errors.New("tool failed")
			handler := toolAdmissionMiddleware(rate.NewLimiter(rate.Inf, 1), make(chan struct{}, 1), client)(
				func(context.Context, string, sdkmcp.Request) (sdkmcp.Result, error) { return nil, toolError },
			)
			start := time.Now()
			_, err = handler(t.Context(), "tools/call", nil)
			require.ErrorIs(t, err, toolError)
			require.Less(t, time.Since(start), 2*time.Second)
		})
	}
}
