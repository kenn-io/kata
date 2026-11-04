package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

const cronIssue = -2

type cronIssueReturn struct {
	previous viewID
	detail   detailModel
}

// Cron links enter synchronously: ordinary detail jumps reject the hidden pane.
func (m Model) cronOpenIssue(uid string) (Model, tea.Cmd) {
	m.cron.issueReturn = &cronIssueReturn{previous: m.prevView, detail: cloneDetailModel(m.detail)}
	m.cron.returnToCron = true
	m.pendingDetailFollow = false
	m.nextDetailFollowGen++
	m = m.seedOpenDetail(Issue{UID: uid, ProjectID: m.scope.projectID})
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

// A missing linked issue keeps ordinary detail error state and a visible notice.
func (m Model) cronIssueFeedback(msg tea.Msg) (Model, tea.Cmd) {
	fetched, ok := msg.(detailFetchedMsg)
	if !ok || !m.cron.returnToCron || fetched.gen != m.detail.gen || fetched.err == nil || m.detail.err == nil {
		return m, nil
	}
	m.toast = &toast{text: sanitizeForLine(m.detail.err.Error()), level: toastError, expiresAt: m.toastNow().Add(5 * time.Second)}
	return m, toastExpireCmd(5 * time.Second)
}
