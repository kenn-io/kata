package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/telemetry"
)

const appOpenedEvent = "app_opened"

// appOpenedGate forwards one queued app_opened per surface per UTC day; it forgets on restart.
type appOpenedGate struct {
	mu   sync.Mutex
	now  func() time.Time
	day  string
	sent map[string]struct{}
}

func newAppOpenedGate(now func() time.Time) *appOpenedGate {
	return &appOpenedGate{now: now, sent: map[string]struct{}{}}
}

// capture calls forward unless surface was already queued today and reports whether it skipped.
// It holds the lock across forward so concurrent first requests wait for the first outcome,
// and marks surface only when forward reports queued.
func (g *appOpenedGate) capture(surface string, forward func() (queued bool)) (skipped bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if day := g.now().UTC().Format(time.DateOnly); day != g.day {
		g.day = day
		g.sent = map[string]struct{}{}
	}
	if _, ok := g.sent[surface]; ok {
		return true
	}
	if forward() {
		g.sent[surface] = struct{}{}
	}
	return false
}

// registerTelemetryHandlers mounts the reporter's capture handler as a typed
// operation, so admission, property filtering and delivery stay with the reporter.
func registerTelemetryHandlers(humaAPI huma.API, cfg ServerConfig) {
	gate := newAppOpenedGate(time.Now)
	huma.Register(humaAPI, huma.Operation{
		OperationID:   "captureTelemetryEvent",
		Method:        http.MethodPost,
		Path:          "/api/v1/ui/telemetry",
		Summary:       "Report a browser telemetry event",
		DefaultStatus: http.StatusAccepted,
		MaxBodyBytes:  16 << 10,
	}, func(ctx context.Context, in *api.CaptureTelemetryEventRequest) (*api.CaptureTelemetryEventResponse, error) {
		if cfg.TelemetryCapture == nil {
			return nil, api.NewError(http.StatusServiceUnavailable, "telemetry_unavailable",
				"telemetry capture is unavailable", "", nil)
		}
		forward := func() (*api.CaptureTelemetryEventResponse, error) {
			raw, err := json.Marshal(in.Body)
			if err != nil {
				return nil, telemetryCaptureFailed()
			}
			request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/ui/telemetry", bytes.NewReader(raw))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			cfg.TelemetryCapture.ServeHTTP(recorder, request)
			switch recorder.Code {
			case http.StatusAccepted:
				out := &api.CaptureTelemetryEventResponse{Status: http.StatusAccepted}
				if err := json.Unmarshal(recorder.Body.Bytes(), &out.Body); err != nil {
					return nil, telemetryCaptureFailed()
				}
				return out, nil
			case http.StatusBadRequest:
				return nil, api.NewError(http.StatusBadRequest, "unsupported_telemetry_event",
					strings.TrimSpace(recorder.Body.String()), "", nil)
			default:
				return nil, telemetryCaptureFailed()
			}
		}
		if strings.TrimSpace(in.Body.Event) != appOpenedEvent {
			return forward()
		}
		surface := telemetry.AppOpenedSurface(in.Body.Properties)
		var out *api.CaptureTelemetryEventResponse
		var err error
		if gate.capture(surface, func() bool {
			out, err = forward()
			return err == nil && out.Body.Status == "queued"
		}) {
			return &api.CaptureTelemetryEventResponse{
				Status: http.StatusAccepted,
				Body:   api.CaptureTelemetryEventResponseBody{Status: "queued"},
			}, nil
		}
		return out, err
	})
}

func telemetryCaptureFailed() error {
	return api.NewError(http.StatusInternalServerError, "telemetry_capture_failed",
		"capture telemetry event failed", "", nil)
}
