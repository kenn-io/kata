package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// reportAppOpened tells the daemon the TUI opened. Init is its only caller, so
// it runs once per launch; the result is dropped so telemetry never delays,
// interrupts or prints into the TUI.
func (m Model) reportAppOpened() tea.Cmd {
	reporter, ok := m.api.(appOpenedAPI)
	if !ok || reporter == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		_ = reporter.ReportAppOpened(ctx)
		return nil
	}
}
