package cron

import (
	"strings"

	"go.kenn.io/kata/internal/tui/cron/records"
)

// PeekState is supplied by the host; rendering never performs issue reads.
type PeekState struct {
	Kind  string
	Issue records.IssuePeek
}

func (m *model) SetPeek(state PeekState) { m.peek = state }

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
	lines := bodyLines{
		{Text: detailMetaStyle.Render("  " + summary)},
		{Text: subtleStyle.Render("  " + plain(issue))},
	}
	if m.peekHeight() == 5 {
		hint := "l run detail"
		if m.currentView() == viewRun {
			hint = "l open job"
		}
		if records.Value(run.IssueUID) != "" {
			hint = "i open issue · " + hint
		}
		hint = "  " + hint
		lines = append(lines, bodyLine{Text: subtleStyle.Render("  " + plain(author))}, bodyLine{Text: detailMetaStyle.Render("  " + plain(comment))}, bodyLine{Text: subtleStyle.Render(hint)})
	}
	return lines
}
