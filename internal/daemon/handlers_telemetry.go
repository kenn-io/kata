package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kit/atomicfile"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/telemetry"
)

// TelemetryReporter admits and captures UI telemetry events; the daemon's
// telemetry.Reporter satisfies it.
type TelemetryReporter interface {
	EventAllowed(event string) bool
	Enabled() bool
	Capture(event string, properties map[string]any) error
}

// appOpenedGate captures one app_opened per surface per UTC day; it forgets on restart.
type appOpenedGate struct {
	mu   sync.Mutex
	now  func() time.Time
	day  string
	sent map[string]struct{}
}

func newAppOpenedGate(now func() time.Time) *appOpenedGate {
	return &appOpenedGate{now: now, sent: map[string]struct{}{}}
}

// capture calls forward unless surface was already captured today. It holds the lock across
// forward so concurrent first requests wait for the first outcome, and marks surface only
// when forward succeeds.
func (g *appOpenedGate) capture(surface string, forward func() (captured bool)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if day := g.now().UTC().Format(time.DateOnly); day != g.day {
		g.day = day
		g.sent = map[string]struct{}{}
	}
	if _, ok := g.sent[surface]; ok {
		return
	}
	if forward() {
		g.sent[surface] = struct{}{}
	}
}

type agentUseGate struct {
	mu    sync.Mutex
	now   func() time.Time
	path  string
	state struct {
		Day   string `json:"day"`
		Count int    `json:"count"`
		Sent  int    `json:"sent"`
	}
}

func newAgentUseGate(path string, now func() time.Time) *agentUseGate {
	g := &agentUseGate{path: path, now: now}
	data, err := os.ReadFile(path) //nolint:gosec // Private telemetry marker derived from Kata home.
	if err == nil {
		_ = json.Unmarshal(data, &g.state)
	}
	return g
}

func (g *agentUseGate) capture(reporter TelemetryReporter) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if day := g.now().UTC().Format(time.DateOnly); day != g.state.Day {
		g.state.Day, g.state.Count, g.state.Sent = day, 0, 0
	}
	if g.state.Count < 101 {
		g.state.Count++
	}
	threshold, bucket := 1, "1-10"
	if g.state.Count > 100 {
		threshold, bucket = 101, "over-100"
	} else if g.state.Count > 10 {
		threshold, bucket = 11, "11-100"
	}
	var err error
	if g.state.Sent < threshold {
		event := "agent_call_count"
		if g.state.Sent == 0 {
			event = "agent_active"
		}
		err = reporter.Capture(event, map[string]any{"call_count_bucket": bucket})
		if err == nil {
			g.state.Sent = threshold
		}
	}
	if g.path != "" {
		if data, marshalErr := json.Marshal(g.state); marshalErr == nil {
			if os.MkdirAll(filepath.Dir(g.path), 0o700) == nil {
				_ = atomicfile.WriteFile(g.path, data, atomicfile.WithPerm(0o600))
			}
		}
	}
	return err
}

// registerTelemetryHandlers lets UI clients report allowlisted events through
// the daemon's reporter, which filters properties and owns delivery.
func registerTelemetryHandlers(humaAPI huma.API, cfg ServerConfig) {
	gate := newAppOpenedGate(time.Now)
	path := ""
	if home, err := config.KataHome(); err == nil {
		path = filepath.Join(home, "telemetry", "agent-use-"+cfg.DB.InstanceUID()+".json")
	}
	agentGate := newAgentUseGate(path, time.Now)
	huma.Register(humaAPI, huma.Operation{
		OperationID:   "captureTelemetryEvent",
		Method:        http.MethodPost,
		Path:          "/api/v1/ui/telemetry",
		Summary:       "Report a browser telemetry event",
		DefaultStatus: http.StatusAccepted,
		MaxBodyBytes:  16 << 10,
	}, func(_ context.Context, in *api.CaptureTelemetryEventRequest) (*api.CaptureTelemetryEventResponse, error) {
		reporter := cfg.Telemetry
		if reporter == nil {
			return nil, api.NewError(http.StatusServiceUnavailable, "telemetry_unavailable",
				"telemetry capture is unavailable", "", nil)
		}
		event := strings.TrimSpace(in.Body.Event)
		if event == "agent_call_count" || !reporter.EventAllowed(event) {
			return nil, api.NewError(http.StatusBadRequest, "unsupported_telemetry_event",
				"unsupported telemetry event", "", nil)
		}
		out := &api.CaptureTelemetryEventResponse{Status: http.StatusAccepted}
		if !reporter.Enabled() {
			out.Body.Status = "disabled"
			return out, nil
		}
		var err error
		capture := func() bool {
			err = reporter.Capture(in.Body.Event, in.Body.Properties)
			return err == nil
		}
		switch event {
		case "agent_active":
			err = agentGate.capture(reporter)
		case "app_opened":
			gate.capture(telemetry.AppOpenedSurface(in.Body.Properties), capture)
		default:
			capture()
		}
		if err != nil {
			return nil, api.NewError(http.StatusInternalServerError, "telemetry_capture_failed",
				"capture telemetry event failed", "", nil)
		}
		out.Body.Status = "queued"
		return out, nil
	})
}
