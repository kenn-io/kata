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
	gen                         uint64
}

type attentionReturnContext struct {
	view, prevView viewID
	detail         detailModel
	focus          focusPane
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
			view:     m.view,
			prevView: m.prevView,
			detail:   cloneDetailModel(m.detail),
			focus:    m.focus,
		}
		m.attention.allProjects = m.scope.allProjects || m.scope.projectID == 0
		m.attention.projectID = m.scope.projectID
		m.attention.projectName = m.scope.projectName
		m.attention.rows = nil
		m.attention.cursor = 0
		m.attention.selectedUID = ""
	}
	m.attention.returnToAttention = false
	m.attention.clearing = false
	m.view = viewAttention
	m.nextDetailFollowGen++
	return m.reloadAttention()
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
	m.attention.clearing = false
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
	m.detail = cloneDetailModel(previous.detail)
	m.view, m.prevView, m.focus = previous.view, previous.prevView, previous.focus
	foreground := previous.view
	if previous.view == viewHelp {
		foreground = previous.prevView
	}
	if m.layout == splitlayout.Split && foreground == viewDetail {
		m.focus = focusDetail
	}
	return m, m.restoredDetailRefetch()
}
func (m Model) routeAttentionViewKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
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
		m.attention.selectedUID = msg.rows[m.attention.cursor].issue.UID
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
	connGen, gen uint64
	uid          string
	err          error
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
	m.attention.clearing = true
	api, ok := m.api.(attentionAPI)
	connGen, gen, actor := m.connGen, m.attention.gen, m.list.actor
	return m, func() tea.Msg {
		if !ok || api == nil {
			return attentionClearedMsg{connGen: connGen, gen: gen, uid: row.issue.UID, err: &mutationPreflightError{errors.New("attention client unavailable")}}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := api.ClearAttentionRequest(ctx, row.issue.ProjectID, row.issue.UID, row.key, row.raw, actor)
		return attentionClearedMsg{connGen: connGen, gen: gen, uid: row.issue.UID, err: err}
	}
}
func (m Model) handleAttentionCleared(msg attentionClearedMsg) (Model, tea.Cmd) {
	if !m.attentionVisible() || m.staleConnMsg(msg.connGen) || msg.gen != m.attention.gen {
		return m, nil
	}
	m.attention.clearing = false
	if msg.err == nil {
		return m.reloadAttention()
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
		next, cmd := m.reloadAttention()
		return next, tea.Batch(cmd, toastExpireCmd(3*time.Second))
	}
	return m, toastExpireCmd(3 * time.Second)
}
