package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kit/tui/helplayout"
)

const cronIssue = -2

type cronIssueReturn struct {
	previous viewID
	detail   detailModel
}

// Cron links enter synchronously: ordinary detail jumps reject the hidden pane.
// Until the read returns, the detail has no issue, so its actions stay inert.
func (m Model) cronOpenIssue(uid string) (Model, tea.Cmd) {
	m.cron.issueReturn = &cronIssueReturn{previous: m.prevView, detail: cloneDetailModel(m.detail)}
	m.cron.returnToCron, m.pendingDetailFollow = true, false
	m.nextDetailFollowGen++
	m = m.seedOpenDetail(Issue{UID: uid, ProjectID: m.scope.projectID})
	m.detail.issue, m.detail.loading = nil, true
	return m, m.detail.fetchDetail(m.api, uid)
}

func (m Model) returnFromCronDetail() (Model, tea.Cmd) {
	m.detail.cancelReads()
	m.cron.returnToCron = false
	m.pendingDetailFollow = false
	m.nextDetailFollowGen++
	m.view = viewCron
	if saved := m.cron.issueReturn; saved != nil {
		m.prevView = saved.previous
		m.detail = m.applyDetailViewportCache(cloneDetailModel(saved.detail))
		m.nextGen++
		m.detail.gen = m.nextGen
		m.cron.issueReturn = nil
	}
	return m, m.restoredDetailRefetch()
}

// The UID read resolves a moved issue through its source project's history.
// Later short-ID reads and actions use the project the issue now belongs to.
func (m Model) cronIssueAdopt(msg tea.Msg) Model {
	fetched, ok := msg.(detailFetchedMsg)
	if ok && m.cron.returnToCron && fetched.gen == m.detail.gen && fetched.err == nil && m.detail.issue != nil && m.detail.issue.ProjectID != 0 {
		m.detail.scopePID = m.detail.issue.ProjectID
	}
	return m
}

// A missing linked issue keeps ordinary detail error state and a visible notice.
func (m Model) cronIssueFeedback(msg tea.Msg) (Model, tea.Cmd) {
	fetched, ok := msg.(detailFetchedMsg)
	if !ok || !m.cron.returnToCron || fetched.gen != m.detail.gen || fetched.err == nil || m.detail.err == nil {
		return m, nil
	}
	m.toast = &toast{text: sanitizeForLine(m.detail.err.Error()), level: toastError, expiresAt: m.toastNow().Add(5 * time.Second)}
	return m, toastExpireCmd(5 * time.Second)
}

// Stacked detail has no pane frame, so a linked issue that failed to load says
// why and keeps the return key visible. With no issue its actions stay inert.
func (m Model) cronDetailView() string {
	dm, chrome := m.detail, m.chrome()
	if !m.cron.returnToCron || dm.loading || dm.issue != nil || dm.err == nil {
		return dm.View(m.width, m.height, chrome)
	}
	rows := [][]helplayout.HelpItem{{{Key: "esc", Description: "back"}}}
	reason := truncate("unavailable: "+sanitizeForLine(dm.err.Error()), max(m.width, 1))
	lines := []string{renderTitleBar(m.width, chrome.scope, chrome.version, chrome.daemon), "", errorStyle.Render(reason)}
	for len(lines) < m.height-helpLines(rows, m.width) {
		lines = append(lines, "")
	}
	return strings.Join(append(lines, renderFooterHelpTable(rows, m.width)), "\n")
}
