package tui

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/api"
)

type authCapabilitiesMsg struct {
	connGen     uint64
	auth        AuthInfo
	instanceUID string
	err         error
}

func (m Model) fetchAuthCapabilities() tea.Cmd {
	apiClient := m.api
	connGen := m.connGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		instance, err := apiClient.GetInstance(ctx)
		return authCapabilitiesMsg{connGen: connGen, auth: instance.Auth, instanceUID: instance.InstanceUID, err: err}
	}
}

func (m Model) handleAuthCapabilities(msg authCapabilitiesMsg) (Model, tea.Cmd) {
	if msg.connGen != m.connGen {
		return m, nil
	}
	m.authCapabilitiesRequired = true
	if msg.err != nil {
		m.authCapabilitiesReady = false
		m.input = inputState{}
		if m.view == viewCredentials && m.credentials.capabilityPending {
			m.credentials.capabilityPending = false
			m.credentials.loading = false
			m.credentials.err = msg.err
			return m, nil
		}
		m.toast = &toast{
			text:  "Daemon permissions unavailable; retry your action to reconnect: " + msg.err.Error(),
			level: toastError, expiresAt: m.toastNow().Add(3 * time.Second),
		}
		return m, toastExpireCmd(3 * time.Second)
	}
	if len(m.undoHistory.entries) > 0 && !reflect.DeepEqual(m.undoHistory.entries[len(m.undoHistory.entries)-1].auth, msg.auth) {
		m.undoHistory.clear("daemon principal changed")
		if m.undoCloseEntryID != 0 {
			m.input = inputState{}
			m.undoCloseEntryID = 0
		}
	}
	if client, ok := m.api.(*undoClient); ok {
		client.mu.Lock()
		client.instance = InstanceInfo{InstanceUID: msg.instanceUID, Auth: msg.auth}
		client.mu.Unlock()
	}
	m.authCapabilitiesReady = true
	m.tokenAuditRead = msg.auth.TokenAuditRead
	m.issueScoped = msg.auth.Scope != nil
	m.scopedWritable = slices.Contains(msg.auth.AllowedActions, "issue.edit")
	m.closeRequiresEvidence = msg.auth.CloseRequiresEvidence
	if !m.inputMutationAllowed() {
		m.input = inputState{}
	}
	if m.view == viewCredentials && m.credentials.capabilityPending {
		m.credentials.capabilityPending = false
		m.credentials.available = m.tokenAuditRead
		m.credentials.loading = m.credentials.available
		if m.credentials.available {
			return m, m.fetchCredentials(m.credentials.gen)
		}
	}
	return m, nil
}

func (m Model) inputMutationAllowed() bool {
	switch m.input.kind {
	case inputNone, inputSearchBar, inputFilterForm:
		return true
	case inputNewIssueForm:
		return m.mutationAllowed(strings.TrimSpace(m.input.fieldValue(fieldParent)) != "")
	case inputParentPrompt:
		return m.mutationAllowed(false)
	default:
		return m.mutationAllowed(true)
	}
}

func (m Model) mutationAllowed(withinSubtree bool) bool {
	if m.authCapabilitiesRequired && !m.authCapabilitiesReady {
		return false
	}
	return !m.issueScoped || (withinSubtree && m.scopedWritable)
}

type evidenceCloseAPI interface {
	CloseWithEvidence(context.Context, int64, string, CloseInput) (*MutationResp, error)
}

func (m Model) routeCloseKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if !m.keymap.Close.matches(msg) {
		return m, nil, false
	}
	target, ok := m.activeCloseTarget()
	if !ok {
		return m, nil, true
	}
	if m.closeRequiresEvidence {
		return m.openCloseForm(target), nil, true
	}
	return m, dispatchLegacyClose(m.api, target, m.list.actor), true
}

func (m Model) activeCloseTarget() (formTarget, bool) {
	if m.detailIsActive() {
		if m.detail.issue == nil {
			return formTarget{}, false
		}
		return formTarget{
			projectID: m.detail.scopePID, issueShortID: m.detail.issue.ShortID,
			detailGen: m.detail.gen, origin: "detail",
		}, true
	}
	if !m.listIsActive() {
		return formTarget{}, false
	}
	issue, ok := m.list.targetRow()
	if !ok {
		return formTarget{}, false
	}
	return formTarget{
		projectID: projectIDForRow(issue, m.scope), issueShortID: issue.ShortID,
		origin: "list",
	}, true
}

func (m Model) openCloseForm(target formTarget) Model {
	m.nextFormGen++
	form := newCloseForm(target)
	form.formGen = m.nextFormGen
	m.input = form
	return m
}

func dispatchLegacyClose(apiClient KataAPI, target formTarget, actor string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := apiClient.Close(ctx, target.projectID, target.issueShortID, actor)
		return mutationDoneMsg{
			origin: target.origin, gen: target.detailGen, kind: "close", resp: resp, err: err,
		}
	}
}

func closeInputFromForm(form inputState, actor string) (CloseInput, error) {
	reason := strings.TrimSpace(form.fieldValue(fieldCloseReason))
	message := strings.TrimSpace(form.fieldValue(fieldCloseMessage))
	if message == "" {
		return CloseInput{}, fmt.Errorf("completion message is required")
	}
	value := strings.TrimSpace(form.fieldValue(fieldEvidenceValue))
	if reason == "wontfix" && value == "" {
		return CloseInput{Actor: actor, Reason: reason, Message: message}, nil
	}
	if value == "" {
		return CloseInput{}, fmt.Errorf("evidence value is required")
	}
	typ := api.EvidenceType(form.fieldValue(fieldEvidenceType))
	evidence := api.Evidence{Type: typ}
	switch typ {
	case api.EvidenceCommit:
		evidence.SHA = value
	case api.EvidencePR:
		evidence.URL = value
	case api.EvidenceTest:
		evidence.Command = value
	case api.EvidenceReviewedPaths:
		for path := range strings.SplitSeq(value, ",") {
			if path = strings.TrimSpace(path); path != "" {
				evidence.Paths = append(evidence.Paths, path)
			}
		}
		if len(evidence.Paths) == 0 {
			return CloseInput{}, fmt.Errorf("reviewed paths are required")
		}
	case api.EvidenceExternal:
		evidence.Account = value
	case api.EvidenceNoChangeAudit:
		evidence.Rationale = value
	case api.EvidenceDuplicateOf, api.EvidenceSupersededBy:
		evidence.IssueRef = value
	default:
		return CloseInput{}, fmt.Errorf("unsupported evidence type %q", typ)
	}
	return CloseInput{
		Actor: actor, Reason: reason, Message: message, Evidence: []api.Evidence{evidence},
	}, nil
}

func dispatchFormClose(
	apiClient any, target formTarget, in CloseInput, formGen int64,
) tea.Cmd {
	return func() tea.Msg {
		closer, ok := apiClient.(evidenceCloseAPI)
		if !ok {
			return mutationDoneMsg{
				origin: "form", kind: "form.close", formGen: formGen,
				err: fmt.Errorf("TUI client does not support evidence-bearing close"),
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := closer.CloseWithEvidence(ctx, target.projectID, target.issueShortID, in)
		return mutationDoneMsg{
			origin: "form", gen: target.detailGen, kind: "close", formGen: formGen,
			resp: resp, err: err,
		}
	}
}
