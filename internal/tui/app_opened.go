package tui

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kit/tui/splitlayout"
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
	if m.width == 0 || (m.width < 80 && m.view != viewProjects && m.view != viewFederation && m.view != viewCredentials) {
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

func (m Model) reportShownError(action string) tea.Cmd {
	kind := map[string]string{"create": "issue_created", "close": "issue_closed"}[action]
	if kind == "" {
		return nil
	}
	return m.reportOutcome("error_shown", kind)
}

func (m Model) reportOutcome(event, kind string) tea.Cmd {
	reporter, ok := m.api.(outcomeAPI)
	if !ok {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		_ = reporter.ReportOutcome(ctx, event, kind)
		return nil
	}
}

// shownError follows an accepted failure through retained pane snapshots.
// Copies share the flag so restoring an already displayed error cannot count it again.
type shownError struct {
	error
	action  string
	counted bool
}

func (e *shownError) Unwrap() error { return e.error }

func (e *shownError) status() string {
	return errorStyle.Render(fmt.Sprintf("%s failed: %s", e.action, e.Error()))
}

func (m Model) countShownError(e *shownError, status string) tea.Cmd {
	if e == nil || e.counted || status != e.status() {
		return nil
	}
	e.counted = true
	return m.reportShownError(e.action)
}

func (m Model) reportVisibleErrors() tea.Cmd {
	if m.width < 80 || m.height < listMinHeight {
		return nil
	}
	switch m.view {
	case viewList:
		return m.reportListError()
	case viewDetail:
		if !m.detail.loading && m.detail.issue != nil && !m.input.kind.isPanelPrompt() {
			return m.countShownError(m.detail.shownError, m.detail.status)
		}
	}
	return nil
}

func (m Model) reportListError() tea.Cmd {
	if m.layout == splitlayout.Split {
		if m.focus == focusDetail {
			return m.countShownError(m.detail.shownError, splitInfoBody(m))
		}
		return m.countShownError(m.list.shownError, splitInfoBody(m))
	}
	if !m.list.loading && m.list.err == nil && !m.input.kind.isCommandBar() {
		return m.countShownError(m.list.shownError, m.list.status)
	}
	return nil
}
