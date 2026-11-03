package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/recurrence"
)

// recurrenceFormState is what a Repeat form needs beyond its fields: the
// revision of the issue and the recurrence it read, and the values it was
// opened with (a form left alone writes nothing).
type recurrenceFormState struct {
	issueRevision int64
	// rec is the recurrence the form read; nil when the issue has none, which
	// makes a save start one.
	rec *recurrence.View
	// rule, start and zone are the values the form was opened with.
	rule, start, zone string
	// currentAfterRefusal describes the recurrence that beat a refused save.
	currentAfterRefusal string
}

const recurrenceChangedMessage = "The issue changed since the form opened; nothing was written. The current recurrence is shown below; save again to write your input over it."

// recurrenceFormLoadedMsg is the issue and recurrence the Repeat form opens
// on, read when O was pressed. formGen ties it to that key press.
type recurrenceFormLoadedMsg struct {
	connGen    uint64
	formGen    int64
	target     formTarget
	issue      Issue
	recurrence *recurrence.View
	err        error
}

func newRecurrenceForm(target formTarget, issue Issue, rec *recurrence.View) inputState {
	s := inputState{
		kind:   inputRecurrenceForm,
		title:  fmt.Sprintf("repeat #%s", target.issueShortID),
		target: target,
	}
	for _, f := range []struct {
		id          fieldID
		label       string
		placeholder string
	}{
		{fieldRule, "Rule", "FREQ=WEEKLY;BYDAY=MO"},
		{fieldStart, "Start", "YYYY-MM-DD"},
		{fieldZone, "Zone", "IANA zone, like Europe/Paris"},
	} {
		input := textinput.New()
		input.Prompt = ""
		input.Placeholder = f.placeholder
		input.Blur()
		s.fields = append(s.fields, inputField{id: f.id, kind: fieldSingleLine, input: input, label: f.label})
	}
	s = s.loadRecurrence(issue, rec)
	_ = s.fields[0].focus()
	return s
}

// loadRecurrence fills the form from the recurrence, or from the issue when it
// has none: the issue's date and the zone kata schedule would pick. The
// revisions a save is guarded on are the ones just read.
func (s inputState) loadRecurrence(issue Issue, rec *recurrence.View) inputState {
	state := recurrenceFormState{issueRevision: issue.Revision, rec: rec}
	if rec != nil {
		state.rule, state.start, state.zone = rec.RRule, rec.DTStart, rec.Timezone
	} else {
		state.start = cmp.Or(issue.ScheduledOnDate, issue.scheduledOn())
		state.zone, _ = recurrence.ResolveZone("", "", issue.timezone(), recurrence.LocalZone)
	}
	s.recurrence = state
	for i := range s.fields {
		switch s.fields[i].id {
		case fieldRule:
			s.fields[i].setValue(state.rule)
		case fieldStart:
			s.fields[i].setValue(state.start)
		case fieldZone:
			s.fields[i].setValue(state.zone)
		}
	}
	return s
}

func (m Model) routeRecurrenceKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if !m.keymap.SetRecurrence.matches(msg) {
		return m, nil, false
	}
	target, _, ok := m.activeIssueTarget()
	if !ok {
		return m, nil, true
	}
	m.nextFormGen++
	return m, readRecurrenceForm(m.api, target, m.nextFormGen, m.connGen), true
}

// readRecurrenceForm reads the issue as the daemon holds it now with its
// recurrence, so the form opens on current values and revisions.
func readRecurrenceForm(api detailAPI, target formTarget, formGen int64, connGen uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		msg := recurrenceFormLoadedMsg{connGen: connGen, formGen: formGen, target: target}
		detail, err := api.GetIssueDetail(ctx, target.projectID, target.issueShortID)
		if err != nil || detail == nil || detail.Issue == nil {
			msg.err = cmp.Or(err, errors.New("the daemon returned no issue"))
			return msg
		}
		msg.issue, msg.recurrence = *detail.Issue, detail.Recurrence
		if reader, ok := api.(recurrenceReadAPI); ok && detail.Issue.RecurrenceID != nil {
			msg.recurrence, msg.err = reader.ReadRecurrence(ctx, target.projectID, detail.Issue.RecurrenceID)
		}
		return msg
	}
}

// openRecurrenceForm opens the form on what O read, unless the user has moved
// on since: another key press, another issue, another daemon.
func (m Model) openRecurrenceForm(msg recurrenceFormLoadedMsg) Model {
	if m.staleConnMsg(msg.connGen) || msg.formGen != m.nextFormGen || m.input.kind != inputNone || m.modal != modalNone {
		return m
	}
	if target, _, ok := m.activeIssueTarget(); !ok || target.issueShortID != msg.target.issueShortID {
		return m
	}
	if msg.err != nil {
		m.toast = &toast{
			text:  "Could not read the recurrence: " + msg.err.Error(),
			level: toastError, expiresAt: m.toastNow().Add(3 * time.Second),
		}
		return m
	}
	m.input = newRecurrenceForm(msg.target, msg.issue, msg.recurrence)
	m.input.formGen = msg.formGen
	return m
}

// routeRecurrenceFormKey handles ctrl+x, the stop key, which asks first.
func (m Model) routeRecurrenceFormKey(msg tea.KeyPressMsg) (Model, bool) {
	if m.input.kind != inputRecurrenceForm || msg.String() != "ctrl+x" || m.input.saving {
		return m, false
	}
	if m.input.recurrence.rec == nil {
		m.input.err = "This issue has no recurrence to stop."
		return m, true
	}
	m.modal = modalStopRecurrence
	return m, true
}

func (m Model) commitRecurrenceForm() (Model, tea.Cmd) {
	start, err := resolveDateInput(m.input.fieldValue(fieldStart), m.toastNow())
	if err != nil {
		m.input.err = "Start " + err.Error() + "."
		return m, nil
	}
	state := m.input.recurrence
	rule, zone := m.input.fieldValue(fieldRule), m.input.fieldValue(fieldZone)
	if state.rec != nil && rule == state.rule && start == state.start && zone == state.zone {
		return m.cancelInput()
	}
	return m.saveRecurrence(RecurrenceInput{
		Actor: m.list.actor, Rule: rule, Start: start, Zone: zone,
		IssueRevision: state.issueRevision, Recurrence: state.rec,
	})
}

// confirmStopRecurrence stops the recurrence the form read, after the user
// said yes. Occurrences already created stay.
func (m Model) confirmStopRecurrence() (Model, tea.Cmd) {
	m.modal = modalNone
	state := m.input.recurrence
	if m.input.kind != inputRecurrenceForm || state.rec == nil {
		return m, nil
	}
	return m.saveRecurrence(RecurrenceInput{
		Actor: m.list.actor, Stop: true, IssueRevision: state.issueRevision, Recurrence: state.rec,
	})
}

func (m Model) saveRecurrence(in RecurrenceInput) (Model, tea.Cmd) {
	m.input.saving = true
	m.input.err = ""
	return m, withConnGen(dispatchFormSaveRecurrence(m.api, m.input.target, in, m.input.formGen), m.connGen)
}

func dispatchFormSaveRecurrence(apiClient any, target formTarget, in RecurrenceInput, formGen int64) tea.Cmd {
	kind := "recurrence.change"
	switch {
	case in.Stop:
		kind = "recurrence.stop"
	case in.Recurrence == nil:
		kind = "recurrence.start"
	}
	return func() tea.Msg {
		writer, ok := apiClient.(recurrenceAPI)
		if !ok {
			return mutationDoneMsg{
				origin: "form", kind: kind, formGen: formGen,
				err: errors.New("TUI client does not support editing recurrences"),
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := writer.SaveRecurrence(ctx, target.projectID, target.issueShortID, in)
		if isRevisionConflict(err) {
			resp = withCurrentRecurrence(ctx, apiClient, target, resp)
		}
		return mutationDoneMsg{
			origin: "form", gen: target.detailGen, kind: kind, formGen: formGen,
			resp: resp, err: err,
		}
	}
}

// withCurrentRecurrence attaches the issue and recurrence as the daemon holds
// them now to a refused save, so the form can show what beat it. When the read
// fails the response is returned unchanged and the form reports the refusal as
// it is.
func withCurrentRecurrence(ctx context.Context, apiClient any, target formTarget, resp *MutationResp) *MutationResp {
	reader, ok := apiClient.(detailAPI)
	if !ok {
		return resp
	}
	detail, err := reader.GetIssueDetail(ctx, target.projectID, target.issueShortID)
	if err != nil || detail == nil || detail.Issue == nil {
		return resp
	}
	if resp == nil {
		resp = &MutationResp{}
	}
	resp.current = &recurrenceSnapshot{issue: *detail.Issue, rec: detail.Recurrence}
	return resp
}

// reloadRecurrenceForm is the answer to a refused save: the form keeps what
// the user typed, shows the recurrence that beat it, and the next save is
// guarded on the revisions just read.
func (m Model) reloadRecurrenceForm(current recurrenceSnapshot) Model {
	typed := [3]string{m.input.fieldValue(fieldRule), m.input.fieldValue(fieldStart), m.input.fieldValue(fieldZone)}
	m.input = m.input.loadRecurrence(current.issue, current.rec)
	m.input.recurrence.currentAfterRefusal = describeRecurrence(current.rec)
	for i, id := range []fieldID{fieldRule, fieldStart, fieldZone} {
		m.input.field(id).setValue(typed[i])
	}
	m.input.saving = false
	m.input.err = recurrenceChangedMessage
	if m.detail.issue != nil && m.detail.issue.UID == current.issue.UID {
		*m.detail.issue = current.issue
		m.detail.recurrence = current.rec
	}
	m, _ = m.refreshListDay(func(lm listModel) listModel { return lm.withCurrentIssue(current.issue) })
	return m
}

func describeRecurrence(rec *recurrence.View) string {
	if rec == nil {
		return "now: no recurrence"
	}
	return fmt.Sprintf("now: %s from %s in %s", rec.RRule, rec.DTStart, rec.Timezone)
}

// renderRecurrenceRow is the detail line for a recurring issue: its rule, the
// next occurrence after the issue's own, and the zone.
func renderRecurrenceRow(rec *recurrence.View, width int) string {
	next := "none"
	if rec.NextOccurrenceKey != nil {
		next = *rec.NextOccurrenceKey
	}
	text := fmt.Sprintf("%s %s · next %s · %s",
		metadataLabel("repeats:"), sanitizeForDisplay(rec.RRule), sanitizeForDisplay(next), sanitizeForDisplay(rec.Timezone))
	return truncate(text, width)
}

// renderRecurrenceForm lays out the three fields, the day the start resolves
// to, and after a refused save the recurrence that beat it.
func renderRecurrenceForm(s inputState, innerW int, now time.Time) string {
	parts := []string{titleStyle.Render(s.title)}
	for idx := range s.fields {
		parts = append(parts, renderNewIssueField(s, idx, innerW))
		if s.fields[idx].id == fieldStart {
			parts = append(parts, subtleStyle.Render(sanitizeForDisplay(datePreview(s.fields[idx].value(), now))))
		}
	}
	if s.recurrence.currentAfterRefusal != "" {
		parts = append(parts, subtleStyle.Render(truncate(sanitizeForDisplay(s.recurrence.currentAfterRefusal), innerW)))
	}
	if status := renderFormStatus(s); status != "" {
		parts = append(parts, status)
	}
	parts = append(parts, renderRecurrenceFooter(s, innerW))
	return modalBoxStyle.Width(innerW+2).Padding(0, 1).Render(strings.Join(parts, "\n"))
}

func renderRecurrenceFooter(s inputState, innerW int) string {
	if s.saving {
		return statusStyle.Render("saving…")
	}
	keys := "ctrl+o save · esc cancel · tab next"
	if s.recurrence.rec != nil {
		keys += " · ctrl+x stop repeating"
	}
	if len(keys) > innerW {
		return subtleStyle.Render("ctrl+o save · esc cancel")
	}
	return subtleStyle.Render(keys)
}
