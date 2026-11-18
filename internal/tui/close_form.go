package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	attentionPending := m.attentionVisible() && m.attention.capabilityPending
	credentialsPending := m.credentialsActive() && m.credentials.capabilityPending
	m.authCapabilitiesRequired = true
	if msg.err != nil {
		m.authCapabilitiesReady = false
		m.activeAuth = AuthInfo{}
		m.activeAuthError = msg.err.Error()
		var apiError *APIError
		m.activeAuthRejected = errors.As(msg.err, &apiError) && apiError != nil && apiError.Status == http.StatusUnauthorized
		m.input = inputState{}
		m = m.closeMoveForm()
		if attentionPending {
			m.attention.capabilityPending = false
			m.attention.loading = false
			m.attention.err = msg.err
		}
		if credentialsPending {
			m.credentials.capabilityPending = false
			m.credentials.loading = false
			m.credentials.err = msg.err
		}
		if attentionPending || credentialsPending {
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
	m.activeAuth = msg.auth
	m.activeAuthError = ""
	m.activeAuthRejected = false
	expiryCmd := m.activeCredentialExpiryCmd()
	m.tokenAuditRead = msg.auth.TokenAuditRead
	m.issueScoped = msg.auth.Scope != nil
	m.scopedWritable = slices.Contains(msg.auth.AllowedActions, "issue.edit")
	m.closeRequiresEvidence = msg.auth.CloseRequiresEvidence
	if !m.inputMutationAllowed() {
		m.input = inputState{}
	}
	if !m.mutationAllowed(false) {
		m = m.closeMoveForm()
	}
	var viewCmd tea.Cmd
	if credentialsPending {
		m.credentials.capabilityPending = false
		m.credentials.available = m.tokenAuditRead
		m.credentials.loading = m.credentials.available
		if m.credentials.available {
			viewCmd = combineCmds(viewCmd, m.fetchCredentials(m.credentials.gen))
		}
	}
	if attentionPending {
		next, cmd := m.reloadAttention()
		m = next
		viewCmd = combineCmds(viewCmd, cmd)
	}
	return m, combineCmds(viewCmd, expiryCmd)
}

func (m Model) inputMutationAllowed() bool {
	switch m.input.kind {
	case inputNone, inputSearchBar, inputFilterForm:
		return true
	case inputNewIssueForm:
		return m.mutationAllowed(strings.TrimSpace(m.input.fieldValue(fieldParent)) != "")
	case inputParentPrompt, inputDeleteForm:
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

// routeCloseKey closes the issue under the cursor. x closes at once unless
// the daemon requires evidence; V always opens the evidence form.
func (m Model) routeCloseKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	withEvidence := m.keymap.CloseWithEvidence.matches(msg)
	if !withEvidence && !m.keymap.Close.matches(msg) {
		return m, nil, false
	}
	target, issue, ok := m.activeIssueTarget()
	if !ok {
		return m, nil, true
	}
	if withEvidence || m.closeRequiresEvidence {
		if target.origin == "list" {
			// Opening a form cancels the priority prompt, as reopen does.
			m.list.pendingPriority = false
			m.list.status = ""
		}
		return m.openCloseForm(target, issue.attachments()), nil, true
	}
	return m, dispatchLegacyClose(m.api, target, m.list.actor), true
}

// activeIssueTarget is the issue a list or detail action key acts on: the open
// detail's issue, else the list row under the cursor.
func (m Model) activeIssueTarget() (formTarget, Issue, bool) {
	if m.detailIsActive() {
		if m.detail.issue == nil {
			return formTarget{}, Issue{}, false
		}
		return formTarget{
			projectID: m.detail.scopePID, issueShortID: m.detail.issue.ShortID,
			detailGen: m.detail.gen, origin: "detail",
		}, *m.detail.issue, true
	}
	if !m.listIsActive() {
		return formTarget{}, Issue{}, false
	}
	issue, ok := m.list.targetRow()
	if !ok {
		return formTarget{}, Issue{}, false
	}
	return formTarget{
		projectID: projectIDForRow(issue, m.scope), issueShortID: issue.ShortID,
		origin: "list",
	}, issue, true
}

func (m Model) openCloseForm(target formTarget, attachments []closeAttachment) Model {
	m.nextFormGen++
	form := newCloseForm(target, attachments)
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

// syncCloseAttachmentField keeps the Attachment radio in step with the
// Reason radio: the close action accepts attachment evidence only for done,
// so any other reason hides the field and a return to done offers it again
// with no choice made.
func (s inputState) syncCloseAttachmentField() inputState {
	if s.kind != inputCloseForm || len(s.attachments) == 0 {
		return s
	}
	offered := s.fieldValue(fieldCloseReason) == "done"
	idx := slices.IndexFunc(s.fields, func(f inputField) bool { return f.id == fieldCloseAttachment })
	switch {
	case offered && idx < 0:
		choices := []string{"none"}
		for _, a := range s.attachments {
			choices = append(choices, a.label())
		}
		s.fields = append(slices.Clone(s.fields), inputField{
			id: fieldCloseAttachment, label: "Attachment evidence", kind: fieldRadio,
			radio: radioField{choices: choices},
		})
	case !offered && idx >= 0:
		s.fields = slices.Delete(slices.Clone(s.fields), idx, idx+1)
		s.active = min(s.active, len(s.fields)-1)
	}
	return s
}

// closeAttachmentChoice is the attachment picked on the form, if any. Only
// done offers the field; a reason change drops it via syncCloseAttachmentField.
func closeAttachmentChoice(form inputState, reason string) (closeAttachment, bool) {
	f := form.field(fieldCloseAttachment)
	if reason != "done" || f == nil || f.radio.index <= 0 || f.radio.index > len(form.attachments) {
		return closeAttachment{}, false
	}
	return form.attachments[f.radio.index-1], true
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
	cited, hasCited := closeAttachmentChoice(form, reason)
	if value == "" && !hasCited {
		return CloseInput{}, fmt.Errorf("evidence value is required")
	}
	var items []api.Evidence
	if value != "" {
		typed, err := typedCloseEvidence(api.EvidenceType(form.fieldValue(fieldEvidenceType)), value)
		if err != nil {
			return CloseInput{}, err
		}
		items = append(items, typed)
	}
	if hasCited {
		items = append(items, api.Evidence{Type: api.EvidenceAttachment, AttachmentID: cited.id})
	}
	return CloseInput{Actor: actor, Reason: reason, Message: message, Evidence: items}, nil
}

// typedCloseEvidence builds the evidence item for the form's type and value.
func typedCloseEvidence(typ api.EvidenceType, value string) (api.Evidence, error) {
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
			return api.Evidence{}, fmt.Errorf("reviewed paths are required")
		}
	case api.EvidenceExternal:
		evidence.Account = value
	case api.EvidenceNoChangeAudit:
		evidence.Rationale = value
	case api.EvidenceDuplicateOf, api.EvidenceSupersededBy:
		evidence.IssueRef = value
	default:
		return api.Evidence{}, fmt.Errorf("unsupported evidence type %q", typ)
	}
	return evidence, nil
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
