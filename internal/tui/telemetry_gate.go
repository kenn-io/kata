package tui

import (
	"context"
	"time"

	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type telemetryGate struct{ clientpkg.TelemetryGate }

func newTelemetryGate() *telemetryGate { return new(telemetryGate) }

type telemetryRequestKey struct{}

func isTelemetryRequest(ctx context.Context) bool { return ctx.Value(telemetryRequestKey{}) == true }

type telemetryClaimKey struct{ target, event, screen string }

func (g *telemetryGate) report(ctx context.Context, key telemetryClaimKey, send func(context.Context) (*generated.CaptureTelemetryEventResponse, error)) error {
	claim := ""
	if key.event == "app_opened" || key.event == "screen_viewed" {
		claim = clientpkg.TelemetryDayMarkerPathForKey(key.target+"\n"+key.event+"\n"+key.screen, "tui-")
	}
	_, err := g.Report(ctx, key.target, claim, time.Now().UTC().Format(time.DateOnly), false, func(ctx context.Context) (*generated.CaptureTelemetryEventResponse, error) {
		return send(context.WithValue(ctx, telemetryRequestKey{}, true))
	})
	return err
}
