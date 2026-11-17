package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// federationNamePromptOpen reports whether the spoke name prompt owns the
// keyboard: it must see every key before the global routing does.
func (m Model) federationNamePromptOpen() bool {
	return m.view == viewFederation && m.federation.mode == federationModeNamePrompt
}

// startFederationName opens the prompt for a spoke row on the list or detail.
// It reads nothing: the row, its hub and this daemon's instance UID are
// already on screen.
func (m Model) startFederationName(row FederationProjectStatus) Model {
	m = m.invalidateFederationOp()
	m.federation.notice = ""
	m.federation.nameDraft = federationNameDraft{
		Return:        m.federation.mode,
		ProjectName:   row.ProjectName,
		HubURL:        row.HubURL,
		AllowInsecure: row.AllowInsecure,
		InstanceUID:   strings.TrimSpace(m.federation.instance.InstanceUID),
	}
	m.federation.mode = federationModeNamePrompt
	return m
}

// startFederationEnrollmentName opens the prompt for the enroll preview's name
// field, starting from the name already chosen.
func (m Model) startFederationEnrollmentName() Model {
	draft := m.federation.draft
	m.federation.op.err = nil
	m.federation.nameDraft = federationNameDraft{
		Return:      federationModePreview,
		ProjectName: draft.SpokeProjectName,
		HubURL:      federationDaemonEndpoint(draft.HubTarget),
		InstanceUID: strings.TrimSpace(m.federation.instance.InstanceUID),
		Input:       draft.SpokeName,
	}
	m.federation.mode = federationModeNamePrompt
	return m
}

func (m Model) routeFederationNamePromptKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		m.detail.cancelReads()
		return m, tea.Quit
	}
	if m.federation.op.running {
		return m, nil
	}
	switch msg.String() {
	case "esc":
		m.federation.op.err = nil
		m.federation.mode = m.federation.nameDraft.Return
		m.federation.nameDraft = federationNameDraft{}
		return m, nil
	case "enter":
		return m.submitFederationName()
	case "backspace":
		if r := []rune(m.federation.nameDraft.Input); len(r) > 0 {
			m.federation.nameDraft.Input = string(r[:len(r)-1])
		}
		m.federation.op.err = nil
		return m, nil
	}
	if msg.Text != "" {
		m.federation.nameDraft.Input += msg.Text
		m.federation.op.err = nil
	}
	return m, nil
}

func (m Model) pasteIntoFederationName(msg tea.PasteMsg) Model {
	if m.federation.op.running {
		return m
	}
	m.federation.nameDraft.Input += strings.TrimRight(msg.Content, "\r\n")
	m.federation.op.err = nil
	return m
}

// submitFederationName checks the name with the daemon's own rules, then either
// keeps it on the enroll draft or sends it to the hub. A refused name keeps
// the prompt and its text.
func (m Model) submitFederationName() (Model, tea.Cmd) {
	draft := m.federation.nameDraft
	if err := draft.validate(); err != nil {
		m.federation.op.err = err
		return m, nil
	}
	if draft.forEnrollment() {
		m.federation.draft.SpokeName = draft.Input
		m.federation.op.err = nil
		m.federation.mode = federationModePreview
		m.federation.nameDraft = federationNameDraft{}
		return m, nil
	}
	m.federation.op = federationOp{
		kind:    federationOpName,
		attempt: m.federation.op.attempt + 1,
		running: true,
	}
	return m, m.executeFederationName(m.federation.op.attempt)
}

func (m Model) executeFederationName(attempt uint64) tea.Cmd {
	connGen := m.connGen
	draft := m.federation.nameDraft
	hubTarget := m.federationLeaveHubTarget(draft.HubURL, draft.AllowInsecure)
	return func() tea.Msg {
		return federationNameResultMsg{
			connGen: connGen,
			attempt: attempt,
			err:     runFederationName(context.Background(), draft, hubTarget),
		}
	}
}

// runFederationName writes the name to the hub the spoke row is bound to,
// with that hub's catalog credential. An empty name clears it.
func runFederationName(parent context.Context, draft federationNameDraft, hubTarget daemonTarget) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	hub, _, err := newFederationHubAdminClient(ctx, hubTarget)
	if err != nil {
		return &mutationPreflightError{err}
	}
	return hub.SetFederationSpokeName(ctx, draft.InstanceUID, draft.Input)
}

// handleFederationNameResult lands a name save. The reply must still belong to
// this daemon connection, the current operation and the open prompt.
func (m Model) handleFederationNameResult(msg federationNameResultMsg) (Model, tea.Cmd) {
	if m.staleConnMsg(msg.connGen) ||
		msg.attempt != m.federation.op.attempt ||
		m.federation.op.kind != federationOpName ||
		!m.federation.op.running ||
		!m.federationNamePromptOpen() {
		return m, nil
	}
	m.federation.op.running = false
	if msg.err != nil {
		m.federation.op.err = federationNameSaveError(msg.err)
		return m, nil
	}
	draft := m.federation.nameDraft
	hub := federationHubDisplay(draft.HubURL)
	if draft.Input == "" {
		m.federation.notice = "cleared the name of this spoke on " + hub
	} else {
		m.federation.notice = fmt.Sprintf("named this spoke %q on %s", draft.Input, hub)
	}
	m.federation.op = federationOp{attempt: m.federation.op.attempt}
	m.federation.mode = draft.Return
	m.federation.nameDraft = federationNameDraft{}
	return m, nil
}

// federationNameSaveError says what is known about a failed save. A refusal
// from the daemon, or a failure before the request left, wrote nothing. Any
// other failure came after the send, so the hub may hold the name; saving the
// same name again is safe.
func federationNameSaveError(err error) error {
	var preflight *mutationPreflightError
	var apiErr *APIError
	if errors.As(err, &preflight) || errors.As(err, &apiErr) {
		return err
	}
	return fmt.Errorf("the hub may have saved this name; save again to check: %w", err)
}
