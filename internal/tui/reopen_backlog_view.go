package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
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
// is placed, so no terminal height lets the modal or a compact view layout hide
// the keys that answer it.
func (m Model) overlayReopenBacklog(body string) string {
	if m.reopenPrompt == nil {
		return body
	}
	width := max(m.width, 1)
	footer := renderFooterHelpTable(modalHelpRows(modalReopenBacklog), width)
	bodyHeight := m.height - lipgloss.Height(footer)
	if bodyHeight < 1 {
		const keys = "  y reopen  n/esc cancel"
		ref := sanitizeForLine(m.reopenPrompt.ref)
		if hash := strings.LastIndex(ref, "#"); hash > 0 {
			ref = "…" + ref[hash:]
		}
		minWidth := lipgloss.Width(fmt.Sprintf("Reopen %s? %d ", ref, m.reopenPrompt.count))
		return truncate(compactReopenBacklog(m.reopenPrompt, max(width-len(keys), minWidth), 1)+keys, width)
	}
	full := renderReopenBacklogModal(m.reopenPrompt)
	if lipgloss.Width(full) <= width && lipgloss.Height(full) <= bodyHeight {
		lines := strings.Split(body, "\n")
		body = strings.Join(lines[:min(bodyHeight, len(lines))], "\n")
		body = overlayModal(body, full, width, bodyHeight)
	} else {
		body = lipgloss.Place(width, bodyHeight, lipgloss.Center, lipgloss.Center, compactReopenBacklog(m.reopenPrompt, width, bodyHeight))
	}
	return body + "\n" + footer
}

// compactReopenBacklog keeps the issue ID when shortening a qualified reference.
func compactReopenBacklog(prompt *reopenPrompt, width, bodyRows int) string {
	ref := sanitizeForLine(prompt.ref)
	line := fmt.Sprintf("Reopen %s? %d recurring child issues.", ref, prompt.count)
	if lipgloss.Width(line) > width && bodyRows > 1 {
		line = fmt.Sprintf("Reopen %s?", ref)
		if cut := lipgloss.Width(line) - width; cut > 0 {
			if hash := strings.LastIndex(ref, "#"); hash > 0 {
				ref = runewidth.TruncateLeft(ref, min(cut, runewidth.StringWidth(ref[:hash])), "…")
				line = fmt.Sprintf("Reopen %s?", ref)
			}
		}
		return truncate(line, width) + "\n" + truncate(fmt.Sprintf("%d recurring child issues", prompt.count), width)
	}
	if cut := lipgloss.Width(line) - width; cut > 0 {
		if hash := strings.LastIndex(ref, "#"); hash > 0 {
			ref = runewidth.TruncateLeft(ref, min(cut, runewidth.StringWidth(ref[:hash])), "…")
			line = fmt.Sprintf("Reopen %s? %d recurring child issues.", ref, prompt.count)
		}
	}
	return truncate(line, width)
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
