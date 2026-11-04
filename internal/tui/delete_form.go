package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// deleteFormState is what a Delete form needs beyond its field: the
// qualified reference to confirm, and the title of the issue it is for.
type deleteFormState struct {
	expected   string
	issueTitle string
}

func newDeleteForm(target formTarget, issue Issue, expected string) inputState {
	input := textinput.New()
	input.Prompt = ""
	input.Blur()
	s := inputState{
		kind:   inputDeleteForm,
		title:  fmt.Sprintf("Type DELETE %s to confirm", expected),
		target: target,
		fields: []inputField{{id: fieldConfirm, kind: fieldSingleLine, input: input, label: "Confirm"}},
		del:    deleteFormState{expected: expected, issueTitle: issue.Title},
	}
	_ = s.fields[0].focus()
	return s
}

// deleteConfirmation uses the daemon's qualified list reference or resolves
// the project for a detail response, which carries only the short ID.
func (m Model) deleteConfirmation(issue Issue) string {
	if issue.QualifiedID != "" {
		return issue.QualifiedID
	}
	project := m.projectsByID[issue.ProjectID]
	if project == "" && issue.ProjectID == m.scope.projectID {
		project = m.scope.projectName
	}
	if project == "" {
		return ""
	}
	return project + "#" + issue.ShortID
}

// deleteHidden reports whether the delete key is not on offer: the connected
// credentials cannot delete. Footer, help overlay and chrome all read it.
func (m Model) deleteHidden() bool {
	return !m.mutationAllowed(false)
}

// helpKeymap is the keymap the help overlay lists: without the delete key
// when it is not on offer.
func (m Model) helpKeymap() keymap {
	km := m.keymap
	if m.deleteHidden() {
		km.Delete = key{}
	}
	return km
}

func (m Model) routeDeleteKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if !m.keymap.Delete.matches(msg) {
		return m, nil, false
	}
	target, issue, ok := m.activeIssueTarget()
	if !ok {
		return m, nil, true
	}
	expected := m.deleteConfirmation(issue)
	if expected == "" {
		next, cmd := m.undoNotice("cannot delete: the issue's project is unknown", toastError)
		return next, cmd, true
	}
	target.issueUID = issue.UID
	m.nextFormGen++
	m.input = newDeleteForm(target, issue, expected)
	m.input.formGen = m.nextFormGen
	return m, nil, true
}

// commitDeleteForm sends the delete once the typed text is the issue's
// "DELETE <project>#<id>"; until then enter does nothing.
func (m Model) commitDeleteForm() (Model, tea.Cmd) {
	if m.input.fieldValue(fieldConfirm) != "DELETE "+m.input.del.expected {
		return m, nil
	}
	m.input.saving = true
	m.input.err = ""
	return m, withConnGen(dispatchFormDelete(m.api, m.input.target, m.input.del.expected, m.list.actor, m.input.formGen), m.connGen)
}

func dispatchFormDelete(apiClient any, target formTarget, qualifiedID, actor string, formGen int64) tea.Cmd {
	return func() tea.Msg {
		writer, ok := apiClient.(deleteAPI)
		if !ok {
			return mutationDoneMsg{
				origin: "form", kind: "delete", formGen: formGen,
				err: errors.New("TUI client does not support deleting issues"),
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := writer.DeleteIssue(ctx, target.projectID, target.issueShortID, qualifiedID, actor)
		return mutationDoneMsg{
			origin: "form", gen: target.detailGen, kind: "delete", formGen: formGen,
			resp: resp, err: err,
		}
	}
}

// finishDelete closes the form after the daemon deleted the issue: the row
// leaves the list, the selection moves to its neighbour, a detail showing it
// is left, and a notice says how to take the delete back.
func (m Model) finishDelete(target formTarget, mut mutationDoneMsg) (Model, tea.Cmd) {
	qualified := m.input.del.expected
	m.input = inputState{}
	uid := target.issueUID
	if mut.resp != nil && mut.resp.Issue != nil && mut.resp.Issue.UID != "" {
		uid = mut.resp.Issue.UID
	}
	var leave tea.Cmd
	if m.detail.issue != nil && m.detail.issue.UID != uid {
		leave = m.refetchOpenDetail()
	} else {
		m, leave = m.leaveDeletedDetail(uid)
	}
	m, follow := m.refreshListDay(func(lm listModel) listModel { return lm.withoutIssue(uid) })
	if m.cache != nil {
		m.cache.markStale()
	}
	message := "deleted " + qualified
	if last := len(m.undoHistory.entries) - 1; (m.view == viewList || m.view == viewDetail) && last >= 0 && m.undoHistory.entries[last].kind == "delete" && m.undoHistory.entries[last].uid == uid {
		message += "; press u to undo"
	}
	m, notice := m.undoNotice(message, toastInfo)
	return m, combineCmds(follow, combineCmds(leave, combineCmds(notice, withConnGen(m.list.refetchCmd(m.api, m.scope), m.connGen))))
}

// leaveDeletedDetail goes Back when the live detail shows the deleted issue.
func (m Model) leaveDeletedDetail(uid string) (Model, tea.Cmd) {
	if m.detail.issue == nil || m.detail.issue.UID != uid {
		return m, nil
	}
	if len(m.detail.navStack) > 0 {
		var cmd tea.Cmd
		m.detail, cmd = m.detail.handleBack(m.api)
		return m, cmd
	}
	m.detail.cancelReads()
	m.detail = newDetailModel()
	next, cmd, _ := m.routeTopLevel(popDetailMsg{})
	return next.(Model), cmd
}

// withoutIssue drops a deleted issue from the list and selects the row after
// it, or the row before it when it was the last.
func (lm listModel) withoutIssue(uid string) listModel {
	rows := lm.visibleRows()
	at := slices.IndexFunc(rows, func(row queueRow) bool { return row.issue.UID == uid })
	lm.issues = slices.DeleteFunc(slices.Clone(lm.issues), func(issue Issue) bool { return issue.UID == uid })
	if at < 0 || lm.selectedUID != uid {
		return lm.restoreCursorToSelection()
	}
	next := at + 1
	if next >= len(rows) {
		next = at - 1
	}
	lm.selectedUID, lm.selectedProjectID = "", 0
	if next >= 0 {
		lm.selectedUID, lm.selectedProjectID = rows[next].issue.UID, rows[next].issue.ProjectID
	}
	return lm.restoreCursorToSelection()
}

// renderDeleteForm shows the issue to be deleted and what to type.
func renderDeleteForm(s inputState, innerW int) string {
	parts := []string{
		titleStyle.Render(truncate(sanitizeForLine(s.title), innerW)),
		subtleStyle.Render(truncate(sanitizeForLine(s.del.issueTitle), innerW)),
		renderNewIssueField(s, 0, innerW),
	}
	if status := renderFormStatus(s); status != "" {
		parts = append(parts, status)
	}
	if s.saving {
		parts = append(parts, statusStyle.Render("deleting…"))
	} else {
		parts = append(parts, subtleStyle.Render("enter delete · esc cancel"))
	}
	return modalBoxStyle.Width(innerW+2).Padding(0, 1).Render(strings.Join(parts, "\n"))
}
