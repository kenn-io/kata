package cron

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"go.kenn.io/kata/internal/tui/cron/records"
)

// PeekState is supplied by the host; rendering never performs issue reads.
type PeekState struct {
	Kind  string
	Issue records.IssuePeek
	// RelatedURL is a safe AgentsView locator recorded when the linked issue
	// closed. It is related issue context, not this run's own session.
	RelatedURL string
}

// OpenRelatedMsg asks the host to open the selected peek's related session.
type OpenRelatedMsg struct{ URL string }

func (m *model) SetPeek(state PeekState) { m.peek = state }

// NotifyError shows a host failure in the status line.
func (m *model) NotifyError(text string) { m.notify(statusError, text) }

// relatedURL is the current peek's locator when it belongs to the selected run.
func (m *model) relatedURL(run records.Run) string {
	if m.peek.RelatedURL == "" || m.peek.Issue.IssueUID == "" || m.peek.Issue.IssueUID != records.Value(run.IssueUID) {
		return ""
	}
	return m.peek.RelatedURL
}

func (m *model) openRelated() tea.Cmd {
	run, ok := m.SelectedRun()
	if !ok {
		return nil
	}
	locator := m.relatedURL(run)
	if locator == "" {
		return nil
	}
	return func() tea.Msg { return OpenRelatedMsg{URL: locator} }
}

// SelectedRun identifies the observation whose context is currently visible.
func (m *model) SelectedRun() (records.Run, bool) {
	if m.currentView() == viewRun {
		return m.detailRun()
	}
	if m.tab != 1 {
		return records.Run{}, false
	}
	r, ok := m.selected()
	if !ok || r.kind != "run" {
		return records.Run{}, false
	}
	return m.data.Runs[r.run], true
}

func (m *model) peekHeight() int {
	if _, ok := m.SelectedRun(); !ok || m.editor != nil {
		return 0
	}
	if m.height >= 18 {
		return 5
	}
	return 2
}

func (m *model) renderPeek() bodyLines {
	run, ok := m.SelectedRun()
	if !ok {
		return nil
	}
	issue := "No issue linked"
	author, comment := "", ""
	if records.Value(run.IssueUID) != "" {
		issue = "Loading issue…"
		switch m.peek.Kind {
		case "unreadable":
			issue = "Issue not readable"
		case "ready", "no_comments":
			if m.peek.Issue.IssueUID == records.Value(run.IssueUID) {
				ref := m.peek.Issue.ShortID
				if ref == "" {
					ref = m.peek.Issue.IssueUID
				}
				issue = strings.TrimSpace(ref + " " + m.peek.Issue.Title)
				if latest := m.peek.Issue.Latest; latest != nil {
					author = strings.TrimSpace(latest.Author + " · " + ago(m.now(), latest.CreatedAt))
					comment = latest.Body
				} else {
					author = "No comments"
				}
			}
		}
	}
	summary := plain(run.Summary.Message)
	if strings.TrimSpace(summary) == "" {
		summary = "No summary recorded"
	}
	related := m.relatedURL(run) != ""
	full := m.peekHeight() == 5
	if related {
		issue += " · Related issue session"
		if !full {
			issue += " · o Open in AgentsView"
		}
	}
	lines := bodyLines{
		{Text: detailMetaStyle.Render("  " + summary)},
		{Text: subtleStyle.Render("  " + plain(issue))},
	}
	if full {
		hint := "l run detail"
		if m.currentView() == viewRun {
			hint = "l open job"
		}
		if records.Value(run.IssueUID) != "" {
			hint = "i open issue · " + hint
		}
		if related {
			hint = "o Open in AgentsView · " + hint
		}
		hint = "  " + hint
		lines = append(lines, bodyLine{Text: subtleStyle.Render("  " + plain(author))}, bodyLine{Text: detailMetaStyle.Render("  " + plain(comment))}, bodyLine{Text: subtleStyle.Render(hint)})
	}
	return lines
}
