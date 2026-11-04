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
	"charm.land/lipgloss/v2"
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/termtext"
)

// recurrenceFormState is what a Repeat form needs beyond its fields: the
// revision of the issue and the recurrence it read, and the values it was
// opened with (a form left alone writes nothing).
type recurrenceFormState struct {
	issueRevision int64
	rec           *recurrenceclient.View
	// rule, start and zone are the values the form was opened with.
	rule, start, zone string
}

func newRecurrenceForm(target formTarget, issue Issue, rec *recurrenceclient.View) inputState {
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
func (s inputState) loadRecurrence(issue Issue, rec *recurrenceclient.View) inputState {
	state := recurrenceFormState{issueRevision: issue.Revision, rec: rec}
	if rec != nil {
		state.rule, state.start, state.zone = rec.RRule, rec.DTStart, rec.Timezone
	} else {
		state.start = cmp.Or(issue.ScheduledOnDate, issue.scheduledOn())
		state.zone, _ = recurrenceclient.ResolveZone("", "", issue.timezone(), localZone)
	}
	s.recurrence, s.target.projectID = state, issue.ProjectID
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
	if !m.detailIsActive() || m.detail.issue == nil {
		return m, nil, true
	}
	target, _, ok := m.activeIssueTarget()
	if !ok {
		return m, nil, true
	}
	m.nextFormGen++
	target.issueUID = m.detail.issue.UID
	m.input = newRecurrenceForm(target, *m.detail.issue, m.detail.recurrence)
	m.input.formGen = m.nextFormGen
	return m, nil, true
}

func (m Model) commitRecurrenceForm() (Model, tea.Cmd) {
	start, err := resolveDateInput(m.input.fieldValue(fieldStart), m.toastNow())
	if err == nil && start == "" {
		m.input.err = "Start is required."
		return m, nil
	}
	if _, dateErr := time.Parse(time.DateOnly, start); err != nil || dateErr != nil {
		m.input.err = "Start must be YYYY-MM-DD, today, tomorrow, +Nd or +Nw."
		return m, nil
	}
	state := m.input.recurrence
	rule, zone := m.input.fieldValue(fieldRule), m.input.fieldValue(fieldZone)
	if state.rec != nil && rule == state.rule && start == state.start && zone == state.zone {
		return m.cancelInput()
	}
	return m.saveRecurrence(RecurrenceInput{
		Rule: rule, Start: start, Zone: zone, Actor: m.list.actor,
		IssueRevision: state.issueRevision, Recurrence: state.rec,
	})
}

func (m Model) saveRecurrence(in RecurrenceInput) (Model, tea.Cmd) {
	m.input.saving = true
	m.input.err = ""
	return m, withConnGen(dispatchFormSaveRecurrence(m.api, m.input.target, in, m.input.formGen), m.connGen)
}

func dispatchFormSaveRecurrence(apiClient any, target formTarget, in RecurrenceInput, formGen int64) tea.Cmd {
	kind := "recurrence.change"
	if in.Recurrence == nil {
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
		resp, err := writer.SaveRecurrence(ctx, target.projectID, target.issueUID, in)
		return mutationDoneMsg{
			origin: "form", gen: target.detailGen, kind: kind, formGen: formGen,
			resp: resp, err: err,
		}
	}
}

// renderRecurrenceRows shows the schedule for a recurring issue: its rule, the
// next occurrence after the issue's own, and the zone.
func renderRecurrenceRows(rec *recurrenceclient.View, width int) []string {
	next := "none"
	if rec.NextOccurrenceKey != nil {
		next = *rec.NextOccurrenceKey
	}
	return joinMetadataRow(
		metadataLabel("repeats:")+" "+sanitizeForDisplay(rec.RRule),
		fmt.Sprintf("next %s · %s", sanitizeForDisplay(next), sanitizeForDisplay(rec.Timezone)), width)
}

// renderRecurrenceForm keeps the fields and controls visible and clamps the
// wrapped status to the remaining rows.
func renderRecurrenceForm(s inputState, innerW, height int, now time.Time) string {
	parts := []string{titleStyle.Render(s.title)}
	for idx := range s.fields {
		label := s.fields[idx].label
		field := renderNewIssueField(s, idx, innerW-lipgloss.Width(label)-1)
		parts = append(parts, strings.Replace(field, "\n", " ", 1))
		if s.fields[idx].id == fieldStart {
			parts = append(parts, subtleStyle.Render(sanitizeForDisplay(datePreview(s.fields[idx].value(), now))))
		}
	}
	footer := renderRecurrenceFooter(s, innerW)
	box := modalBoxStyle.Width(innerW+4).Padding(0, 1)
	if status := renderFormStatus(s); status != "" {
		rows := max(height-lipgloss.Height(box.Render(strings.Join(append(parts, footer), "\n"))), 0)
		lines := termtext.Wrap(status, innerW)
		parts = append(parts, lines[:min(len(lines), rows)]...)
	}
	parts = append(parts, footer)
	return box.Render(strings.Join(parts, "\n"))
}

func renderRecurrenceFooter(s inputState, innerW int) string {
	if s.saving {
		return statusStyle.Render("saving…")
	}
	var controls []string
	for _, item := range recurrenceFormControls() {
		controls = append(controls, item.Key+" "+item.Description)
	}
	keys := strings.Join(controls, " · ")
	if len(keys) > innerW {
		return subtleStyle.Render("ctrl+o save · esc cancel")
	}
	return subtleStyle.Render(keys)
}

func recurrenceFormControls() []helplayout.HelpItem {
	return []helplayout.HelpItem{
		{Key: "ctrl+o", Description: "save"},
		{Key: "esc", Description: "cancel"},
		{Key: "tab", Description: "field"},
	}
}
