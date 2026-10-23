package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kit/tui/splitlayout"
)

type attentionState struct {
	rows                        []attentionRequest
	cursor                      int
	messageScroll               int
	selectedUID, recipient      string
	recipientErr                error
	allProjects                 bool
	projectID                   int64
	projectName                 string
	loading, capabilityPending  bool
	err                         error
	skipped                     int
	clearing, returnToAttention bool
	returnView                  viewID
	returnContext               *attentionReturnContext
	gen, clearGen               uint64
}

type attentionReturnContext struct {
	view, prevView, foreground viewID
	detail                     detailModel
	focus                      focusPane
}

type attentionLoadedMsg struct {
	connGen, gen uint64
	rows         []attentionRequest
	skipped      int
	err          error
}

func resolveAttentionRecipient(env string, auth AuthInfo, actor string) (string, error) {
	raw := env
	if strings.TrimSpace(raw) == "" {
		raw = auth.Actor
		if strings.TrimSpace(raw) == "" {
			raw = actor
		}
	}
	return notification.NormalizeRecipient(raw)
}

func (m Model) transitionToAttention() (Model, tea.Cmd) {
	if m.view != viewAttention && !m.attention.returnToAttention && m.attention.returnContext == nil {
		m.attention.returnView = m.view
		m.attention.returnContext = &attentionReturnContext{
			view:       m.view,
			prevView:   m.prevView,
			foreground: m.attentionForeground(),
			detail:     cloneDetailModel(m.detail),
			focus:      m.focus,
		}
		m.attention.allProjects = m.scope.allProjects || m.scope.projectID == 0
		m.attention.projectID = m.scope.projectID
		m.attention.projectName = m.scope.projectName
		m.attention.rows = nil
		m.attention.cursor = 0
		m.attention.messageScroll = 0
		m.attention.skipped = 0
		m.attention.selectedUID = ""
	}
	m.attention.returnToAttention = false
	m.view = viewAttention
	m.nextDetailFollowGen++
	return m.reloadAttention()
}

func (m Model) attentionForeground() viewID {
	if m.view == viewHelp {
		return m.prevView
	}
	if m.view != viewList && m.view != viewDetail {
		return m.view
	}
	if m.layout == splitlayout.Split {
		if m.focus == focusDetail {
			return viewDetail
		}
		return viewList
	}
	return m.view
}

func (m Model) syncAttentionReturnDetail(msg tea.Msg) Model {
	if m.attention.returnContext == nil || !isDetailFetchMsg(msg) {
		return m
	}
	updated, applied := m.attention.returnContext.detail.applyFetchedIfFresh(msg)
	if !applied {
		return m
	}
	context := *m.attention.returnContext
	context.detail = cloneDetailModel(updated)
	m.attention.returnContext = &context
	return m
}

func (m Model) syncAttentionReturnMutation(mut mutationDoneMsg) Model {
	if m.attention.returnContext == nil || m.staleConnMsg(mut.connGen) {
		return m
	}
	updated, applied := m.attention.returnContext.detail.applyMutationState(mut)
	if !applied {
		return m
	}
	context := *m.attention.returnContext
	context.detail = cloneDetailModel(updated)
	m.attention.returnContext = &context
	return m
}

func attentionUsesSavedPreviousView(view viewID) bool {
	switch view {
	case viewDaemons, viewFederation, viewCredentials:
		return true
	default:
		return false
	}
}

func (m Model) reloadAttention() (Model, tea.Cmd) {
	m.attention.gen++
	m.attention.loading = true
	m.attention.err = nil
	env := os.Getenv("KATA_INBOX_USER")
	m.attention.recipient, m.attention.recipientErr = resolveAttentionRecipient(env, m.activeAuth, m.list.actor)
	if m.attention.recipientErr != nil {
		m.attention.loading = false
		m.attention.err = m.attention.recipientErr
		return m, nil
	}
	m.attention.capabilityPending = strings.TrimSpace(env) == "" && m.authCapabilitiesRequired && !m.authCapabilitiesReady
	if m.attention.capabilityPending {
		return m, m.fetchAuthCapabilities()
	}
	return m, m.fetchAttentionRequests(m.attention.gen)
}
func (m Model) leaveAttentionView() Model {
	m.attention.gen++
	m.attention.loading = false
	m.attention.capabilityPending = false
	m.attention.returnToAttention = false
	return m
}
func (m Model) prepareAttentionGlobalExit() (Model, tea.Cmd) {
	m.attention.returnToAttention = false
	if m.attention.returnContext == nil {
		if m.view == viewAttention || (m.view == viewHelp && m.prevView == viewAttention) {
			target := m.attention.returnView
			m = m.leaveAttentionView()
			if target == viewAttention {
				target = viewList
			}
			m.view = target
		}
		return m, nil
	}
	previous := *m.attention.returnContext
	m = m.leaveAttentionView()
	m.attention.returnContext = nil
	m.detail = m.applyDetailViewportCache(cloneDetailModel(previous.detail))
	m.view, m.prevView, m.focus = previous.view, previous.prevView, previous.focus
	foreground := previous.foreground
	if m.layout == splitlayout.Stacked && attentionUsesSavedPreviousView(previous.view) &&
		previous.prevView == viewList && previous.focus == focusDetail && m.detail.issue != nil {
		m.prevView = viewDetail
	}
	if m.layout == splitlayout.Split && attentionUsesSavedPreviousView(previous.view) &&
		(previous.prevView == viewDetail || previous.focus == focusDetail) {
		m.focus = focusDetail
	}
	if previous.view != viewHelp {
		switch foreground {
		case viewDetail:
			m.focus = focusDetail
			if m.layout == splitlayout.Stacked {
				m.view = viewDetail
			}
		case viewList:
			m.focus = focusList
			if m.layout == splitlayout.Stacked {
				m.view = viewList
			}
		}
	}

	var detailCmd tea.Cmd
	listOwnsDetail := (foreground == viewList ||
		(foreground != viewDetail && previous.prevView == viewList)) &&
		previous.focus != focusDetail && len(m.detail.navStack) == 0
	if m.layout == splitlayout.Split && listOwnsDetail {
		if issue, ok := pickHighlightedIssue(m.list); !ok {
			m.nextDetailFollowGen++
			m.detail = m.applyDetailViewportCache(newDetailModel())
		} else {
			pid := detailProjectID(issue, m.scope)
			if !detailModelMatches(&m.detail, issue.UID, pid) {
				m, detailCmd = m.scheduleDetailFollow()
			} else {
				detailCmd = m.restoredDetailRefetch()
			}
		}
	} else {
		detailCmd = m.restoredDetailRefetch()
	}
	if m.view == viewProjects {
		detailCmd = combineCmds(detailCmd, m.fetchProjectsWithStats())
	}
	return m, detailCmd
}

// prepareAttentionListNavigation clears the Attention detail return path
// when the user moves into the split list pane. A Projects-origin Attention
// view has no browsing scope, so initialize an all-projects list before
// allowing that navigation to leave an empty list loading forever.
func (m Model) prepareAttentionListNavigation() (Model, tea.Cmd) {
	var cmd tea.Cmd
	if m.attention.returnToAttention && m.scope.projectID == 0 && !m.scope.allProjects {
		m.scopeGen++
		m.scope = scope{allProjects: true, scopeGen: m.scopeGen}
		actor := m.list.actor
		m.list = newListModel()
		m.list.actor = actor
		m.cache.markStale()
		m.detail.allProjects = true
		cmd = m.fetchInitial()
	}
	m.attention.returnToAttention = false
	m.attention.returnContext = nil
	return m, cmd
}

func (m Model) routeAttentionViewKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.keymap.PageUp.matches(msg) || m.keymap.PageDown.matches(msg) {
		if len(m.attention.rows) > 0 {
			lines, height := attentionMessageViewport(m.attention, max(m.width, 1), attentionBodyBudget(m))
			step := max(height, 1)
			if m.keymap.PageUp.matches(msg) {
				step = -step
			}
			start := min(m.attention.messageScroll, max(len(lines)-height, 0))
			m.attention.messageScroll = min(max(start+step, 0), max(len(lines)-height, 0))
		}
		return m, nil
	}
	previousCursor := m.attention.cursor
	switch msg.String() {
	case "enter":
		return m.openAttentionIssue()
	case "esc":
		return m.prepareAttentionGlobalExit()
	case "c":
		return m.clearAttentionRequest()
	case "r":
		if m.attention.clearing {
			return m, nil
		}
		return m.reloadAttention()
	case "j", "down":
		m.attention.cursor = min(m.attention.cursor+1, max(len(m.attention.rows)-1, 0))
	case "k", "up":
		m.attention.cursor = max(m.attention.cursor-1, 0)
	case "g", "home":
		m.attention.cursor = 0
	case "G", "end":
		m.attention.cursor = max(len(m.attention.rows)-1, 0)
	}
	if m.attention.cursor != previousCursor {
		m.attention.messageScroll = 0
	}
	if len(m.attention.rows) > 0 {
		m.attention.selectedUID = m.attention.rows[m.attention.cursor].issue.UID
	}
	return m, nil
}
func (m Model) fetchAttentionRequests(gen uint64) tea.Cmd {
	api, ok := m.api.(attentionAPI)
	connGen := m.connGen
	recipient := m.attention.recipient
	var pid *int64
	if !m.attention.allProjects {
		pid = new(m.attention.projectID)
	}
	return func() tea.Msg {
		if !ok || api == nil {
			return attentionLoadedMsg{connGen: connGen, gen: gen, err: errors.New("attention client unavailable")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, skipped, err := api.ListAttentionRequests(ctx, pid, recipient)
		return attentionLoadedMsg{connGen: connGen, gen: gen, rows: rows, skipped: skipped, err: err}
	}
}
func (m Model) attentionVisible() bool {
	return m.view == viewAttention || (m.view == viewHelp && m.prevView == viewAttention)
}
func (m Model) handleAttentionLoaded(msg attentionLoadedMsg) (Model, tea.Cmd) {
	if !m.attentionVisible() || m.staleConnMsg(msg.connGen) || msg.gen != m.attention.gen {
		return m, nil
	}
	m.attention.loading = false
	m.attention.err = msg.err
	if msg.err != nil {
		return m, nil
	}
	var previous attentionRequest
	if len(m.attention.rows) > 0 {
		previous = m.attention.rows[m.attention.cursor]
	}
	m.attention.rows = msg.rows
	m.attention.skipped = msg.skipped
	m.attention.cursor = min(m.attention.cursor, max(len(msg.rows)-1, 0))
	for i, row := range msg.rows {
		if row.issue.UID == m.attention.selectedUID {
			m.attention.cursor = i
			break
		}
	}
	m.attention.selectedUID = ""
	if len(msg.rows) > 0 {
		selected := msg.rows[m.attention.cursor]
		m.attention.selectedUID = selected.issue.UID
		if selected.issue.UID != previous.issue.UID || selected.message != previous.message || string(selected.raw) != string(previous.raw) {
			m.attention.messageScroll = 0
		}
	} else {
		m.attention.messageScroll = 0
	}
	return m, nil
}

func (m Model) openAttentionIssue() (Model, tea.Cmd) {
	if len(m.attention.rows) == 0 {
		return m, nil
	}
	row := m.attention.rows[m.attention.cursor]
	m.attention.returnToAttention = true
	m = m.seedOpenDetail(row.issue)
	// An empty browsing scope (the Projects screen) also means all projects
	// for Attention, even though the hidden list has no project selected.
	if m.attention.allProjects {
		m.detail.scopePID = row.issue.ProjectID
		m.detail.allProjects = true
	}
	return m, m.fetchOpenDetail()
}
func (m Model) returnFromAttentionDetail() (Model, tea.Cmd) {
	return m.transitionToAttention()
}

type attentionClearedMsg struct {
	connGen, clearGen uint64
	uid               string
	err               error
}

func (m Model) clearAttentionRequest() (Model, tea.Cmd) {
	if len(m.attention.rows) == 0 || m.attention.clearing {
		return m, nil
	}
	if !m.mutationAllowed(true) {
		if m.authCapabilitiesRequired && !m.authCapabilitiesReady {
			m.toast = &toast{text: "Checking daemon permissions; retry your action when connected", level: toastInfo, expiresAt: m.toastNow().Add(3 * time.Second)}
			return m, tea.Batch(m.fetchAuthCapabilities(), toastExpireCmd(3*time.Second))
		}
		return m, nil
	}
	row := m.attention.rows[m.attention.cursor]
	m.attention.gen++
	m.attention.clearGen++
	m.attention.clearing = true
	api, ok := m.api.(attentionAPI)
	connGen, clearGen, actor := m.connGen, m.attention.clearGen, m.list.actor
	return m, func() tea.Msg {
		if !ok || api == nil {
			return attentionClearedMsg{connGen: connGen, clearGen: clearGen, uid: row.issue.UID, err: &mutationPreflightError{errors.New("attention client unavailable")}}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := api.ClearAttentionRequest(ctx, row.issue.ProjectID, row.issue.UID, row.key, row.raw, actor)
		return attentionClearedMsg{connGen: connGen, clearGen: clearGen, uid: row.issue.UID, err: err}
	}
}
func (m Model) handleAttentionCleared(msg attentionClearedMsg) (Model, tea.Cmd) {
	if m.staleConnMsg(msg.connGen) || !m.attention.clearing || msg.clearGen != m.attention.clearGen {
		return m, nil
	}
	m.attention.clearing = false
	if msg.err == nil {
		if m.attentionVisible() {
			return m.reloadAttention()
		}
		return m, nil
	}
	var apiErr *APIError
	var preflight *mutationPreflightError
	isAPI := errors.As(msg.err, &apiErr)
	conflict := isAPI && apiErr != nil && apiErr.Status == 409 && apiErr.Code == "metadata_guard_failed"
	text := sanitizeForLine(msg.err.Error())
	if conflict {
		text = "Request changed; review it before clearing"
	}
	m.toast = &toast{text: text, level: toastError, expiresAt: m.toastNow().Add(3 * time.Second)}
	// A transport failure may have happened after commit. Reconcile, without
	// retrying a write whose outcome is unknown or claiming it succeeded.
	if conflict || (!isAPI && !errors.As(msg.err, &preflight)) {
		if m.attentionVisible() {
			next, cmd := m.reloadAttention()
			return next, tea.Batch(cmd, toastExpireCmd(3*time.Second))
		}
	}
	return m, toastExpireCmd(3 * time.Second)
}
