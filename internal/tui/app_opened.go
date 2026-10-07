package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/telemetry"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kit/telemetry/posthog"
)

// reportAppOpened tells the daemon the TUI opened. Init is its only caller, so
// it runs once per launch; the result is dropped so telemetry never delays,
// interrupts or prints into the TUI.
func (m Model) reportAppOpened() tea.Cmd {
	reporter, ok := m.api.(appOpenedAPI)
	if !ok {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		_ = reporter.ReportAppOpened(ctx)
		return nil
	}
}

func (m Model) reportSessionEnded(elapsed time.Duration) {
	if !posthog.EnabledFromEnv("KATA") || m.activeDaemon.resolved.BaseURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	hc, err := client.NewHTTPClientForResolved(ctx, m.activeDaemon.resolved, client.Opts{Timeout: time.Second})
	if err != nil {
		return
	}
	defer hc.CloseIdleConnections()
	apiClient, err := kataclient.NewWithHTTPClient(m.activeDaemon.resolved.BaseURL, hc)
	if err != nil {
		return
	}
	_, _ = apiClient.CaptureTelemetryEventWithResponse(ctx, &generated.CaptureTelemetryEventRequestOptions{
		Body: &generated.CaptureTelemetryEventBody{Event: "session_ended", Properties: map[string]any{"surface": "tui", "duration_bucket": telemetry.DurationBucket(elapsed)}},
	})
}
