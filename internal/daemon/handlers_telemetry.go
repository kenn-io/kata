package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
)

// registerTelemetryHandlers mounts the reporter's capture handler as a typed
// operation, so admission, property filtering and delivery stay with the reporter.
func registerTelemetryHandlers(humaAPI huma.API, cfg ServerConfig) {
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
	})
}

func telemetryCaptureFailed() error {
	return api.NewError(http.StatusInternalServerError, "telemetry_capture_failed",
		"capture telemetry event failed", "", nil)
}
