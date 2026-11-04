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
	"go.kenn.io/kit/tui/termtext"
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
		title:  fmt.Sprintf("DELETE %s", expected),
		target: target,
		fields: []inputField{{id: fieldConfirm, kind: fieldSingleLine, input: input, label: "Confirm"}},
		del:    deleteFormState{expected: expected, issueTitle: issue.Title},
	}
	_ = s.fields[0].focus()
	return s
}

// deleteHidden keeps deletion hints aligned with the current credentials.
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
	if m.view != viewList && m.view != viewDetail {
		return m, nil, true
	}
	target, issue, ok := m.activeIssueTarget()
	if !ok {
		return m, nil, true
	}
	expected, ok := m.deleteConfirmation(issue)
	if !ok {
		next, cmd := m.undoNotice("Cannot delete issue: project name is unknown", toastError)
		return next, cmd, true
	}
	target.issueUID = issue.UID
	m.nextFormGen++
	m.input = newDeleteForm(target, issue, expected)
	m.input.formGen = m.nextFormGen
	return m, nil, true
}

func (m Model) deleteConfirmation(issue Issue) (string, bool) {
	if issue.QualifiedID != "" {
		return issue.QualifiedID, true
	}
	return m.qualifiedIssueRef(issue.ProjectID, issue.ShortID)
}

func (m Model) qualifiedIssueRef(projectID int64, shortID string) (string, bool) {
	project := m.projectsByID[projectID]
	if project == "" && projectID == m.scope.projectID {
		project = m.scope.projectName
	}
	if project == "" || shortID == "" {
		return "", false
	}
	return project + "#" + shortID, true
}

// commitDeleteForm sends the delete once the typed text is the issue's
// "DELETE <project>#<id>"; until then enter does nothing.
func (m Model) commitDeleteForm() (Model, tea.Cmd) {
	if strings.TrimSpace(m.input.fieldValue(fieldConfirm)) != "DELETE "+m.input.del.expected {
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
// leaves the list, the selection moves to its neighbour, and a notice says
// how to take the delete back.
func (m Model) finishDelete(target formTarget) (Model, tea.Cmd) {
	qualified := m.input.del.expected
	m.input = inputState{}
	uid := target.issueUID
	m, forgotten := m.forgetDeletedIssue(uid)
	m, follow := m.refreshListDay(func(lm listModel) listModel { return lm.withoutIssue(uid) })
	m, detailCmd := m.maybeBootstrapSplitDetail()
	if m.cache != nil {
		m.cache.markStale()
	}
	message := "deleted " + qualified
	if last := len(m.undoHistory.entries) - 1; m.undoOffered() && last >= 0 && m.undoHistory.entries[last].kind == "delete" && m.undoHistory.entries[last].uid == uid {
		message += "; press u to undo"
	}
	m, notice := m.undoNotice(message, toastInfo)
	return m, tea.Batch(forgotten, follow, detailCmd, notice, withConnGen(m.list.refetchCmd(m.api, m.scope), m.connGen))
}

// forgetDeletedIssue invalidates the snapshots that navigation can restore.
func (m Model) forgetDeletedIssue(uid string) (Model, tea.Cmd) {
	deleted := func(dm detailModel) bool { return dm.issue != nil && dm.issue.UID == uid }
	forgetDetail := func(dm detailModel, pop func(detailModel) (detailModel, tea.Cmd)) (detailModel, tea.Cmd, bool) {
		dm.navStack = slices.DeleteFunc(slices.Clone(dm.navStack), deleted)
		for i := range dm.navStack {
			dm.navStack[i] = dm.navStack[i].invalidated()
		}
		if !deleted(dm) {
			return dm, nil, false
		}
		if len(dm.navStack) > 0 {
			next, cmd := pop(dm)
			return next, cmd, false
		}
		dm.cancelReads()
		return newDetailModel(), nil, true
	}
	forget := func(dm detailModel, focus *focusPane, views ...*viewID) detailModel {
		dm, _, reset := forgetDetail(dm, detailModel.handleBack)
		if reset {
			*focus = focusList
			for _, view := range views {
				if *view == viewDetail {
					*view = viewList
				}
			}
		}
		return dm.invalidated()
	}
	if m.inboxReturn != nil {
		previous := *m.inboxReturn
		previous.list = previous.list.withoutIssue(uid)
		previous.detail = forget(previous.detail, &previous.focus, &previous.view, &previous.prevView)
		m.inboxReturn = &previous
	}
	if m.attention.returnContext != nil {
		previous := *m.attention.returnContext
		previous.detail = forget(previous.detail, &previous.focus, &previous.view, &previous.prevView, &previous.foreground)
		m.attention.returnContext = &previous
	}
	m.attention.setRows(slices.DeleteFunc(slices.Clone(m.attention.rows), func(row attentionRequest) bool { return row.issue.UID == uid }))
	wasDeleted := deleted(m.detail)
	detail, cmd, reset := forgetDetail(m.detail, func(dm detailModel) (detailModel, tea.Cmd) {
		next, cmd, _ := dm.handleNavKey(tea.KeyPressMsg{Code: tea.KeyEscape}, m.keymap, m.api)
		return next, cmd
	})
	m.detail = detail
	if reset {
		return m.leaveDetail()
	}
	if wasDeleted {
		return m, withConnGen(cmd, m.connGen)
	}
	if slices.ContainsFunc(m.detail.children, func(child Issue) bool { return child.UID == uid }) {
		return m, withConnGen(m.detail.refetch(m.api), m.connGen)
	}
	return m, nil
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
		titleStyle.Render(strings.Join(termtext.Wrap(sanitizeForLine(s.title), innerW-2), "\n")),
		subtleStyle.Render(truncate(sanitizeForLine(s.del.issueTitle), innerW-2)),
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
