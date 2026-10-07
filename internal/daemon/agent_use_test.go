package daemon

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type agentTelemetry struct {
	fakeTelemetryReporter
	events []string
}

func (*agentTelemetry) EventAllowed(string) bool { return true }
func (r *agentTelemetry) Capture(event string, props map[string]any) error {
	if err := r.fakeTelemetryReporter.Capture(event, props); err != nil {
		return err
	}
	r.events = append(r.events, event)
	return nil
}

// The daily contract counts observations across restarts and emits only reached buckets.
func TestAgentUseDailyThresholds(t *testing.T) {
	now := time.Date(2026, 10, 2, 23, 59, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "telemetry", "agent-use")
	clock := func() time.Time { return now }
	gate := newAgentUseGate(path, clock)
	r := &agentTelemetry{}
	for call := 1; call <= 102; call++ {
		if call == 10 || call == 100 {
			gate = newAgentUseGate(path, clock)
		}
		require.NoError(t, gate.capture(r))
		switch call {
		case 1, 2, 10:
			require.Equal(t, []string{"agent_active"}, r.events)
		case 11, 100:
			require.Equal(t, []string{"agent_active", "agent_call_count"}, r.events)
		case 101, 102:
			require.Equal(t, []string{"agent_active", "agent_call_count", "agent_call_count"}, r.events)
		}
	}
	require.Equal(t, []map[string]any{{"call_count_bucket": "1-10"}, {"call_count_bucket": "11-100"}, {"call_count_bucket": "over-100"}}, r.captured)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if os.PathSeparator != '\\' {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	past := time.Unix(1, 0)
	require.NoError(t, os.Chtimes(path, past, past))
	require.NoError(t, gate.capture(r))
	info, err = os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, past, info.ModTime(), "saturated state is not rewritten")
	now = now.Add(time.Minute).In(time.FixedZone("west", -5*60*60))
	require.NoError(t, gate.capture(r))
	require.Equal(t, "agent_active", r.events[3])
	require.Equal(t, map[string]any{"call_count_bucket": "1-10"}, r.captured[3])
}

func TestAgentUseCaptureFailureRetriesCurrentBucket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-use")
	r := &agentTelemetry{}
	r.failNext = true
	gate := newAgentUseGate(path, time.Now)
	require.Error(t, gate.capture(r))
	gate = newAgentUseGate(path, time.Now)
	require.NoError(t, gate.capture(r))
	for range 8 {
		require.NoError(t, gate.capture(r))
	}
	r.failNext = true
	require.Error(t, gate.capture(r))
	gate = newAgentUseGate(path, time.Now)
	require.NoError(t, gate.capture(r))
	require.Equal(t, []string{"agent_active", "agent_call_count"}, r.events)
	require.Equal(t, "11-100", r.captured[1]["call_count_bucket"])
	for range 88 {
		require.NoError(t, gate.capture(r))
	}
	require.Len(t, r.events, 2, "retry counts the current observation once")
	require.NoError(t, gate.capture(r))
	require.Len(t, r.events, 3)
}

func TestAgentTelemetryRouteOwnsBuckets(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	r := &agentTelemetry{}
	server := newTelemetryTestServer(t, r, Principal{Kind: PrincipalWebLocal})
	for range 11 {
		response := server.post(t.Context(), t, `{"event":"agent_active","properties":{"call_count_bucket":"over-100","path":"/example","actor":"example-agent"}}`)
		require.Equal(t, 202, response.Code, response.Body.String())
	}
	require.Equal(t, []string{"agent_active", "agent_call_count"}, r.events)
	require.Equal(t, []map[string]any{{"call_count_bucket": "1-10"}, {"call_count_bucket": "11-100"}}, r.captured)
	response := server.post(t.Context(), t, `{"event":"agent_call_count"}`)
	require.Equal(t, 400, response.Code)
	disabled := newTelemetryTestServer(t, newDisabledReporter(t), Principal{Kind: PrincipalWebLocal})
	response = disabled.post(t.Context(), t, `{"event":"agent_active"}`)
	require.Equal(t, 202, response.Code)
	require.JSONEq(t, `{"status":"disabled"}`, response.Body.String())
}

func TestAgentUseConcurrentObservations(t *testing.T) {
	gate := newAgentUseGate(filepath.Join(t.TempDir(), "agent-use"), time.Now)
	r := &agentTelemetry{}
	var calls sync.WaitGroup
	for range 101 {
		calls.Go(func() { require.NoError(t, gate.capture(r)) })
	}
	calls.Wait()
	require.Equal(t, 101, gate.state.Count)
	require.Equal(t, []string{"agent_active", "agent_call_count", "agent_call_count"}, r.events)
}

func TestAgentUseFirstCaptureRetriesReachedBucket(t *testing.T) {
	gate := newAgentUseGate(filepath.Join(t.TempDir(), "agent-use"), time.Now)
	r := &agentTelemetry{}
	for range 11 {
		r.failNext = true
		require.Error(t, gate.capture(r))
	}
	require.NoError(t, gate.capture(r))
	require.Equal(t, []string{"agent_active", "agent_call_count"}, r.events)
	require.Equal(t, []map[string]any{{"call_count_bucket": "1-10"}, {"call_count_bucket": "11-100"}}, r.captured)
}
