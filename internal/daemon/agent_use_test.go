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
	t.Run("thresholds and restart", func(t *testing.T) {
		now := time.Date(2026, 10, 2, 23, 59, 0, 0, time.UTC)
		path := filepath.Join(t.TempDir(), "telemetry", "agent-use")
		clock := func() time.Time { return now }
		gate := newAgentUseGate(path, clock)
		r := &agentTelemetry{}
		for call := 1; call <= 102; call++ {
			if call == 10 || call == 100 || call == 102 {
				gate = newAgentUseGate(path, clock)
			}
			full := captureAgentUse(t, gate, r)
			require.Equal(t, call >= 101, full)
			switch call {
			case 1, 2, 10:
				require.Equal(t, []string{"agent_active"}, r.events)
			case 11, 100:
				require.Equal(t, []string{"agent_active", "agent_call_count"}, r.events)
			case 101, 102:
				require.Equal(t, []string{"agent_active", "agent_call_count", "agent_call_count"}, r.events)
			}
		}
		require.Equal(t, []capturedTelemetryEvent{{event: "agent_active", properties: map[string]any{"call_count_bucket": "1-10"}}, {event: "agent_call_count", properties: map[string]any{"call_count_bucket": "11-100"}}, {event: "agent_call_count", properties: map[string]any{"call_count_bucket": "over-100"}}}, r.captured)
		info, err := os.Stat(path)
		require.NoError(t, err)
		if os.PathSeparator != '\\' {
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		}
		past := time.Unix(1, 0)
		require.NoError(t, os.Chtimes(path, past, past))
		captureAgentUse(t, gate, r)
		info, err = os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, past, info.ModTime(), "saturated state is not rewritten")
		now = now.Add(time.Minute).In(time.FixedZone("west", -5*60*60))
		require.False(t, captureAgentUse(t, gate, r))
		require.Equal(t, "agent_active", r.events[3])
		require.Equal(t, map[string]any{"call_count_bucket": "1-10"}, r.captured[3].properties)
	})
	t.Run("capture failure", func(t *testing.T) {
		t.Run("current bucket", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent-use")
			r := &agentTelemetry{}
			r.failNext = true
			gate := newAgentUseGate(path, time.Now)
			_, err := gate.capture(r)
			require.Error(t, err)
			gate = newAgentUseGate(path, time.Now)
			captureAgentUse(t, gate, r)
			for range 8 {
				captureAgentUse(t, gate, r)
			}
			r.failNext = true
			_, err = gate.capture(r)
			require.Error(t, err)
			gate = newAgentUseGate(path, time.Now)
			captureAgentUse(t, gate, r)
			require.Equal(t, []string{"agent_active", "agent_call_count"}, r.events)
			require.Equal(t, "11-100", r.captured[1].properties["call_count_bucket"])
			for range 88 {
				captureAgentUse(t, gate, r)
			}
			require.Len(t, r.events, 2, "retry counts the current observation once")
			r.failNext = true
			full, err := gate.capture(r)
			require.Error(t, err)
			require.False(t, full)
			require.True(t, captureAgentUse(t, gate, r))
			require.Len(t, r.events, 3)
		})
		t.Run("first capture catch-up", func(t *testing.T) {
			gate := newAgentUseGate(filepath.Join(t.TempDir(), "agent-use"), time.Now)
			r := &agentTelemetry{}
			for range 11 {
				r.failNext = true
				_, err := gate.capture(r)
				require.Error(t, err)
			}
			captureAgentUse(t, gate, r)
			require.Equal(t, []string{"agent_active", "agent_call_count"}, r.events)
			require.Equal(t, []capturedTelemetryEvent{{event: "agent_active", properties: map[string]any{"call_count_bucket": "1-10"}}, {event: "agent_call_count", properties: map[string]any{"call_count_bucket": "11-100"}}}, r.captured)
		})
	})
	t.Run("concurrent observations", func(t *testing.T) {
		gate := newAgentUseGate(filepath.Join(t.TempDir(), "agent-use"), time.Now)
		r := &agentTelemetry{}
		var calls sync.WaitGroup
		for range 101 {
			calls.Go(func() { captureAgentUse(t, gate, r) })
		}
		calls.Wait()
		require.Equal(t, 101, gate.state.Count)
		require.Equal(t, []string{"agent_active", "agent_call_count", "agent_call_count"}, r.events)
	})
}

func TestAgentTelemetryRouteOwnsBuckets(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	r := &agentTelemetry{}
	server := newTelemetryTestServer(t, r, Principal{Kind: PrincipalWebLocal})
	response := server.post(t.Context(), t, `{"event":"agent_active","properties":{"call_count_bucket":"over-100","path":"/example","actor":"example-agent"}}`)
	require.Equal(t, 202, response.Code, response.Body.String())
	require.JSONEq(t, `{"status":"queued"}`, response.Body.String())
	require.Equal(t, []string{"agent_active"}, r.events)
	require.Equal(t, []capturedTelemetryEvent{{event: "agent_active", properties: map[string]any{"call_count_bucket": "1-10"}}}, r.captured)
	for call := 2; call <= 101; call++ {
		response = server.post(t.Context(), t, `{"event":"agent_active"}`)
		require.Equal(t, 202, response.Code)
	}
	require.JSONEq(t, `{"status":"full"}`, response.Body.String())
	response = server.post(t.Context(), t, `{"event":"agent_call_count"}`)
	require.Equal(t, 400, response.Code)
	disabled := newTelemetryTestServer(t, newDisabledReporter(t), Principal{Kind: PrincipalWebLocal})
	response = disabled.post(t.Context(), t, `{"event":"agent_active"}`)
	require.Equal(t, 202, response.Code)
	require.JSONEq(t, `{"status":"disabled"}`, response.Body.String())
}

func captureAgentUse(t *testing.T, gate *agentUseGate, reporter TelemetryReporter) bool {
	t.Helper()
	full, err := gate.capture(reporter)
	require.NoError(t, err)
	return full
}
