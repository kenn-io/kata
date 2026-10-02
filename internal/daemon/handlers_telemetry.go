package daemon

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
)

// TelemetryReporter admits and captures UI telemetry events; the daemon's
// telemetry.Reporter satisfies it.
type TelemetryReporter interface {
	EventAllowed(event string) bool
	Enabled() bool
	Capture(event string, properties map[string]any) error
}

// registerTelemetryHandlers lets UI clients report allowlisted events through
// the daemon's reporter, which filters properties and owns delivery.
func registerTelemetryHandlers(humaAPI huma.API, cfg ServerConfig) {
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
		if err := reporter.Capture(in.Body.Event, in.Body.Properties); err != nil {
			return nil, api.NewError(http.StatusInternalServerError, "telemetry_capture_failed",
				"capture telemetry event failed", "", nil)
		}
		out.Body.Status = "queued"
		return out, nil
	})
}
