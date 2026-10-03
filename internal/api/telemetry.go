package api //nolint:revive // package name "api" is the public wire namespace.

// CaptureTelemetryEventRequest carries one browser telemetry event.
type CaptureTelemetryEventRequest struct {
	Body CaptureTelemetryEventRequestBody
}

// CaptureTelemetryEventRequestBody names the event and its optional
// properties. The daemon's reporter drops properties its allowlist omits.
type CaptureTelemetryEventRequestBody struct {
	Event      string  `json:"event" minLength:"1"`
	Properties JSONMap `json:"properties,omitempty"`
}

// CaptureTelemetryEventResponse reports whether the event was queued for
// delivery or accepted while telemetry is disabled.
type CaptureTelemetryEventResponse struct {
	Status int
	Body   CaptureTelemetryEventResponseBody
}

// CaptureTelemetryEventResponseBody is the 202 body.
type CaptureTelemetryEventResponseBody struct {
	Status string `json:"status" enum:"queued,disabled"`
}
