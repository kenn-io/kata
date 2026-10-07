package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
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
	reporter, ok := m.api.(sessionEndedAPI)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = reporter.ReportSessionEnded(ctx, elapsed)
}
