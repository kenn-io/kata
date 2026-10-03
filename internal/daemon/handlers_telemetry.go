package daemon

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
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

// registerTelemetryHandlers lets UI clients report allowlisted events through
// the daemon's reporter, which filters properties and owns delivery.
func registerTelemetryHandlers(humaAPI huma.API, cfg ServerConfig) {
	gate := newAppOpenedGate(time.Now)
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
		if !reporter.EventAllowed(in.Body.Event) {
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
		if strings.TrimSpace(in.Body.Event) == "app_opened" {
			gate.capture(telemetry.AppOpenedSurface(in.Body.Properties), capture)
		} else {
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
