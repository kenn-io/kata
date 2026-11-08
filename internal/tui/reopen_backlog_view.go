package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"go.kenn.io/kit/tui/termtext"
)

// reopenPrompt is the pending confirmation for a reopen that would deliver a
// backlog. undoEntryID is non-zero when the reopen is the undo of a close.
type reopenPrompt struct {
	target      formTarget
	ref         string
	count       int
	undoEntryID uint64
}

// reopenBacklogMsg carries the backlog count for a reopen the user asked for.
// A lookup that failed arrives as a count of zero.
type reopenBacklogMsg struct {
	connGen uint64
	target  formTarget
	count   int
}

// activeIssueStatus is the status of the issue a list or detail key acts on.
func (m Model) activeIssueStatus() string {
	if m.detailIsActive() {
		if m.detail.issue == nil {
			return ""
		}
		return m.detail.issue.Status
	}
	if issue, ok := m.list.targetRow(); ok {
		return issue.Status
	}
	return ""
}

// routeReopenKey reopens the issue under the cursor. A closed issue first has
// its recurring backlog counted so the user can confirm a large delivery.
func (m Model) routeReopenKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if !m.keymap.Reopen.matches(msg) {
		return m, nil, false
	}
	// Help and other full-screen views hide the selection the key would act on.
	if m.view != viewList && m.view != viewDetail {
		return m, nil, true
	}
	// Reopen has one owner at a time; a lookup or undo already holds it.
	if m.reopenLookupPending || m.undoInFlight {
		return m, nil, true
	}
	target, _, ok := m.activeIssueTarget()
	if !ok {
		return m, nil, true
	}
	if target.origin == "list" {
		// Like any key after `!`, this one cancels the priority prompt.
		m.list.pendingPriority = false
		m.list.status = ""
	}
	checker, ok := m.api.(reopenBacklogAPI)
	// An open parent may be mid-backlog; reopening it delivers nothing.
	if !ok || m.activeIssueStatus() == "open" {
		return m, m.dispatchReopen(target), true
	}
	// The pending lookup owns reopen until its answer arrives.
	m.reopenLookupPending = true
	connGen := m.connGen
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		count, err := checker.ReopenBacklog(ctx, target.projectID, target.issueShortID)
		if err != nil {
			count = 0
		}
		return reopenBacklogMsg{connGen: connGen, target: target, count: count}
	}, true
}

// promptReopenBacklog asks the user to confirm a reopen that delivers a
// backlog. A modal, form or Jobs filter that took control while the lookup ran
// keeps it: the prompt is dropped, or its keys would answer a question the user
// never saw. Help and the other full-screen views hide the issue the prompt
// names, so an answer that lands there is dropped too; an undo keeps its entry
// and asks again on the next undo.
func (m Model) promptReopenBacklog(prompt reopenPrompt) Model {
	if !m.canQuit() || (m.view != viewList && m.view != viewDetail) {
		return m
	}
	m.reopenPrompt = &prompt
	m.modal = modalReopenBacklog
	return m
}

func (m Model) handleReopenBacklog(msg reopenBacklogMsg) (Model, tea.Cmd) {
	m.reopenLookupPending = false
	if m.staleConnMsg(msg.connGen) {
		return m, nil
	}
	if current, _, ok := m.activeIssueTarget(); !ok || current != msg.target {
		return m, nil
	}
	if msg.count > 1 {
		return m.promptReopenBacklog(reopenPrompt{target: msg.target, ref: "#" + msg.target.issueShortID, count: msg.count}), nil
	}
	return m, m.dispatchReopen(msg.target)
}

func (m Model) dispatchReopen(target formTarget) tea.Cmd {
	apiClient, actor := m.api, m.list.actor
	if target.origin == "detail" {
		actor = m.detail.actor
	}
	return withConnGen(func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := apiClient.Reopen(ctx, target.projectID, target.issueShortID, actor)
		return mutationDoneMsg{origin: target.origin, gen: target.detailGen, kind: "reopen", resp: resp, err: err}
	}, m.connGen)
}

// confirmReopenBacklog proceeds with the reopen the user accepted.
func (m Model) confirmReopenBacklog() (Model, tea.Cmd) {
	prompt := m.reopenPrompt
	m.modal, m.reopenPrompt = modalNone, nil
	if prompt == nil {
		return m, nil
	}
	if prompt.undoEntryID == 0 {
		return m, m.dispatchReopen(prompt.target)
	}
	return m.runUndo(undoOptions{evidenceRequired: m.closeRequiresEvidence}, prompt.undoEntryID)
}

func (m Model) cancelReopenBacklog() Model {
	m.modal, m.reopenPrompt = modalNone, nil
	return m
}

// overlayReopenBacklog draws the reopen confirmation over body with its
// choices on a footer row of their own. The footer is reserved before the modal
// is placed, keeping the keys visible from 40 columns and 2 rows up.
func (m Model) overlayReopenBacklog(body string) string {
	width := max(m.width, 1)
	footer := renderFooterHelpTable(modalHelpRows(modalReopenBacklog, movePicking), width)
	rows := m.height - lipgloss.Height(footer)
	if rows < 1 {
		return footer
	}
	full := renderReopenBacklogModal(m.reopenPrompt)
	var top string
	if lipgloss.Width(full) <= width && lipgloss.Height(full) <= rows {
		top = overlayModal(body, full, width, rows)
	} else {
		top = lipgloss.Place(width, rows, lipgloss.Center, lipgloss.Center, compactReopenBacklog(m.reopenPrompt, width, rows))
	}
	lines := strings.Split(top, "\n")
	return strings.Join(lines[:min(rows, len(lines))], "\n") + "\n" + footer
}

// compactReopenBacklog keeps the issue ID and backlog count within the available rows.
func compactReopenBacklog(prompt *reopenPrompt, width, rows int) string {
	ref := sanitizeForLine(prompt.ref)
	bare := ref[max(strings.LastIndex(ref, "#"), 0):]
	warning := fmt.Sprintf("%d recurring child issues", prompt.count)
	var text string
	for _, candidate := range []string{
		fmt.Sprintf("Reopen %s?\n%s", ref, warning),
		fmt.Sprintf("Reopen %s?\n%s", bare, warning),
		ref + ": " + warning,
		bare + ": " + warning,
	} {
		text = candidate
		if lipgloss.Height(text) <= rows && lipgloss.Width(text) <= width {
			break
		}
	}
	lines := termtext.Wrap(text, width)
	return strings.Join(lines[:min(rows, len(lines))], "\n")
}

func renderReopenBacklogModal(prompt *reopenPrompt) string {
	if prompt == nil {
		return ""
	}
	body := strings.Join([]string{
		fmt.Sprintf("Reopen %s?", sanitizeForLine(prompt.ref)),
		"",
		fmt.Sprintf("Reopening delivers %d recurring child", prompt.count),
		"issues that came due while it was closed.",
		"",
		"To change how missed runs are handled,",
		"use `kata cron job update`.",
	}, "\n")
	return modalBoxStyle.Render(body)
}
