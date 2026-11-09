package tui

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
	"go.kenn.io/kit/tui/termtext"
)

// recurrenceFormState is what a Repeat form needs beyond its fields: the
// revision of the issue and the recurrence it read.
type recurrenceFormState struct {
	issueRevision int64
	rec           *recurrenceclient.View
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
	state := recurrenceFormState{issueRevision: issue.Revision, rec: rec}
	var rule, start, zone string
	if rec != nil {
		rule, start, zone = rec.RRule, rec.DTStart, rec.Timezone
	} else {
		start = cmp.Or(issue.ScheduledOnDate, issue.scheduledOn())
		zone, _ = recurrenceclient.ResolveZone("", "", issue.timezone(), localZone)
	}
	s.recurrence, s.target.projectID = state, issue.ProjectID
	for i := range s.fields {
		switch s.fields[i].id {
		case fieldRule:
			s.fields[i].setValue(rule)
		case fieldStart:
			s.fields[i].setValue(start)
		case fieldZone:
			s.fields[i].setValue(zone)
		}
	}
	_ = s.fields[0].focus()
	return s
}

func (m Model) routeRecurrenceKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if !m.keymap.SetRecurrence.matches(msg) {
		return m, nil, false
	}
	if !m.detailPaneVisible() || m.detail.issue == nil {
		return m, nil, false
	}
	if m.detail.reads != nil && m.detail.reads.remaining&1 != 0 {
		return m, nil, true
	}
	if m.detail.recurrenceErr != nil {
		m.toast = &toast{
			text:  "Could not read the repeat: " + m.detail.recurrenceErr.Error(),
			level: toastInfo, expiresAt: m.toastNow().Add(toastNoBindingTTL),
		}
		return m, toastExpireCmd(toastNoBindingTTL), true
	}
	if m.detail.issue.RecurrenceID != nil && m.detail.recurrence == nil {
		m.toast = &toast{
			text:  "Can't read this issue's repeat; it may have been stopped",
			level: toastInfo, expiresAt: m.toastNow().Add(toastNoBindingTTL),
		}
		return m, toastExpireCmd(toastNoBindingTTL), true
	}
	target, _, _ := m.activeIssueTarget()
	m.nextFormGen++
	target.issueUID = m.detail.issue.UID
	m.input = newRecurrenceForm(target, *m.detail.issue, m.detail.recurrence)
	m.input.formGen = m.nextFormGen
	return m, nil, true
}

func (m Model) commitRecurrenceForm() (Model, tea.Cmd) {
	state := m.input.recurrence
	rule, zone := strings.TrimSpace(m.input.fieldValue(fieldRule)), m.input.fieldValue(fieldZone)
	if state.rec != nil && rule == "" {
		return m.saveRecurrence(RecurrenceInput{Stop: true, Recurrence: state.rec, Actor: m.list.actor})
	}
	start, err := resolveDateInput(m.input.fieldValue(fieldStart), m.toastNow())
	if err == nil && start == "" {
		m.input.err = "Start is required."
		return m, nil
	}
	if _, dateErr := time.Parse(time.DateOnly, start); err != nil || dateErr != nil {
		m.input.err = "Start must be YYYY-MM-DD, today, tomorrow, +Nd or +Nw."
		return m, nil
	}
	return m.saveRecurrence(RecurrenceInput{
		Rule: rule, Start: start, Zone: zone, Actor: m.list.actor,
		IssueRevision: state.issueRevision, Recurrence: state.rec,
	})
}

func (m Model) saveRecurrence(in RecurrenceInput) (Model, tea.Cmd) {
	m.input.saving = true
	m.input.err = ""
	return m, withConnGen(dispatchFormSaveRecurrence(m.api.(recurrenceAPI), m.input.target, in, m.input.formGen), m.connGen)
}

func dispatchFormSaveRecurrence(apiClient recurrenceAPI, target formTarget, in RecurrenceInput, formGen int64) tea.Cmd {
	kind := "recurrence.change"
	if in.Recurrence == nil {
		kind = "recurrence.start"
	} else if in.Stop {
		kind = "recurrence.stop"
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := apiClient.SaveRecurrence(ctx, target.projectID, target.issueUID, in)
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
		if s.fields[idx].id == fieldRule && s.recurrence.rec != nil {
			parts = append(parts, subtleStyle.Render("blank rule stops repeating"))
		}
		if s.fields[idx].id == fieldStart {
			parts = append(parts, subtleStyle.Render(sanitizeForDisplay(datePreview(s.fields[idx].value(), now))))
		}
	}
	footer := renderRecurrenceFooter(s, innerW)
	box := modalBoxStyle.Width(innerW+4).Padding(0, 1)
	if status := renderFormStatus(s); status != "" {
		rows := max(height-lipgloss.Height(box.Render(strings.Join(append(parts, footer), "\n"))), 0)
		lines := termtext.Wrap(status, innerW)
		if rows > 1 && len(lines) > rows {
			lines = append(lines[:1], lines[len(lines)-rows+1:]...)
		}
		parts = append(parts, lines[:min(len(lines), rows)]...)
	}
	parts = append(parts, footer)
	return box.Render(strings.Join(parts, "\n"))
}

func renderRecurrenceFooter(s inputState, innerW int) string {
	var controls []string
	for _, item := range saveCancelFieldControls() {
		controls = append(controls, item.Key+" "+item.Description)
	}
	return renderFormFooterHint(s, innerW, strings.Join(controls, " · "))
}
