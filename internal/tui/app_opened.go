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

func (m Model) screenName() string {
	if m.width < 80 {
		return ""
	}
	switch m.view {
	case viewHelp:
		return "help"
	case viewEmpty:
		return "empty"
	case viewProjects:
		return "projects"
	case viewDaemons:
		return "daemons"
	case viewFederation:
		return "federation"
	case viewCredentials:
		return "credentials"
	}
	if m.detailIsActive() {
		return "issue"
	}
	if m.scope.inbox {
		return "inbox"
	}
	return "issues"
}

func (m Model) reportScreenViewed(screen string) tea.Cmd {
	reporter, ok := m.api.(screenViewedAPI)
	if !ok || screen == "" {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		_ = reporter.ReportScreenViewed(ctx, screen)
		return nil
	}
}

func (m *Model) screenViewCommand(visit bool) tea.Cmd {
	screen := m.screenName()
	day := time.Now().UTC().Format(time.DateOnly)
	if screen == "" || m.api == nil {
		return nil
	}
	if screen == m.telemetryScreen && m.connGen == m.telemetryConnGen && (day == m.telemetryDay || !visit) {
		return nil
	}
	m.telemetryScreen, m.telemetryConnGen, m.telemetryDay = screen, m.connGen, day
	return m.reportScreenViewed(screen)
}
