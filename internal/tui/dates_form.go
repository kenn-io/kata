package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/api"
)

// DatesInput is one dates save: the keys to write and the revision the form
// read. A nil value clears its key.
type DatesInput struct {
	Actor           string
	IfMatchRevision int64
	Set             map[string]*string
}

type datesAPI interface {
	SetDates(ctx context.Context, projectID int64, ref string, in DatesInput) (*MutationResp, error)
}

// datesFormState is what a Dates form needs beyond its fields: the revision it
// read, the raw dates it was opened with (a field left alone is never written,
// so a stored time keeps its time), and the clock the previews count from.
type datesFormState struct {
	revision  int64
	scheduled string
	deadline  string
	now       time.Time
}

const datesChangedMessage = "The issue changed since the form opened; nothing was written. The form now shows the current dates."

func newDatesForm(target formTarget, issue Issue) inputState {
	s := inputState{
		kind:   inputDatesForm,
		title:  fmt.Sprintf("dates of #%s", target.issueShortID),
		target: target,
	}
	for _, f := range []struct {
		id    fieldID
		label string
	}{{fieldScheduled, "Scheduled"}, {fieldDeadline, "Deadline"}} {
		input := textinput.New()
		input.Prompt = ""
		input.Placeholder = "none"
		input.Blur()
		s.fields = append(s.fields, inputField{id: f.id, kind: fieldSingleLine, input: input, label: f.label})
	}
	s = s.loadDates(issue)
	_ = s.fields[0].focus()
	return s
}

// loadDates fills the form from issue and records the revision a save is
// guarded on.
func (s inputState) loadDates(issue Issue) inputState {
	s.dates.revision = issue.Revision
	s.dates.scheduled, s.dates.deadline = issue.scheduledOn(), issue.deadlineOn()
	for i := range s.fields {
		switch s.fields[i].id {
		case fieldScheduled:
			s.fields[i].setValue(s.dates.scheduled)
		case fieldDeadline:
			s.fields[i].setValue(s.dates.deadline)
		}
	}
	return s
}

// datesInputFromForm reads the form into the keys a save writes. A field left
// as opened writes nothing; a refused field is reported by label.
func datesInputFromForm(s inputState, now time.Time, actor string) (DatesInput, error) {
	in := DatesInput{Actor: actor, IfMatchRevision: s.dates.revision, Set: map[string]*string{}}
	for _, f := range []struct {
		id     fieldID
		label  string
		key    string
		opened string
	}{
		{fieldScheduled, "Scheduled", api.MetadataKeyScheduledOn, s.dates.scheduled},
		{fieldDeadline, "Deadline", api.MetadataKeyDeadlineOn, s.dates.deadline},
	} {
		text := s.fieldValue(f.id)
		if text == f.opened {
			continue
		}
		value, err := resolveDateInput(text, now)
		if err != nil {
			return DatesInput{}, errors.New(f.label + " " + err.Error() + ".")
		}
		if value == f.opened {
			continue
		}
		if value == "" {
			in.Set[f.key] = nil
		} else {
			in.Set[f.key] = &value
		}
	}
	return in, nil
}

func (m Model) routeDatesKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if !m.keymap.SetDates.matches(msg) {
		return m, nil, false
	}
	target, issue, ok := m.activeIssueTarget()
	if !ok {
		return m, nil, true
	}
	m.nextFormGen++
	m.input = newDatesForm(target, issue)
	m.input.formGen = m.nextFormGen
	return m, nil, true
}

func (m Model) commitDatesForm() (Model, tea.Cmd) {
	in, err := datesInputFromForm(m.input, m.toastNow(), m.list.actor)
	if err != nil {
		m.input.err = err.Error()
		return m, nil
	}
	if len(in.Set) == 0 {
		return m.cancelInput()
	}
	m.input.saving = true
	m.input.err = ""
	return m, withConnGen(dispatchFormSetDates(m.api, m.input.target, in, m.input.formGen), m.connGen)
}

func dispatchFormSetDates(apiClient any, target formTarget, in DatesInput, formGen int64) tea.Cmd {
	return func() tea.Msg {
		writer, ok := apiClient.(datesAPI)
		if !ok {
			return mutationDoneMsg{
				origin: "form", kind: "dates", formGen: formGen,
				err: errors.New("TUI client does not support editing dates"),
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := writer.SetDates(ctx, target.projectID, target.issueShortID, in)
		if isRevisionConflict(err) {
			resp = withCurrentIssue(ctx, apiClient, target, resp)
		}
		return mutationDoneMsg{
			origin: "form", gen: target.detailGen, kind: "dates", formGen: formGen,
			resp: resp, err: err,
		}
	}
}

func isRevisionConflict(err error) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && apiErr != nil && apiErr.Status == http.StatusPreconditionFailed
}

// withCurrentIssue attaches the issue as the daemon holds it now to a refused
// save, so the form can show the values that beat it. When the read fails the
// response is returned unchanged and the form reports the refusal as it is.
func withCurrentIssue(ctx context.Context, apiClient any, target formTarget, resp *MutationResp) *MutationResp {
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
	resp.Issue = detail.Issue
	return resp
}

// reloadDatesForm is the answer to a refused save: the form stays open, shows
// the current dates, and the next save is guarded on the revision just read.
func (m Model) reloadDatesForm(current Issue) Model {
	m.input = m.input.loadDates(current)
	m.input.saving = false
	m.input.err = datesChangedMessage
	return m
}

// withDatePreviews stamps the clock the previews count from.
func (s inputState) withDatePreviews(now time.Time) inputState {
	s.dates.now = now
	return s
}

// renderDatesForm lays out the two date fields, each with the day its text
// resolves to underneath.
func renderDatesForm(s inputState, innerW int) string {
	parts := []string{titleStyle.Render(s.title)}
	for idx := range s.fields {
		parts = append(parts, renderNewIssueField(s, idx, innerW))
		preview := ""
		if !s.dates.now.IsZero() {
			preview = datePreview(s.fields[idx].value(), s.dates.now)
		}
		parts = append(parts, subtleStyle.Render(sanitizeForDisplay(preview)))
	}
	if status := renderFormStatus(s); status != "" {
		parts = append(parts, status)
	}
	parts = append(parts, renderDatesFooter(s))
	return modalBoxStyle.Width(innerW+2).Padding(0, 1).Render(strings.Join(parts, "\n"))
}

func renderDatesFooter(s inputState) string {
	if s.saving {
		return statusStyle.Render("saving…")
	}
	return subtleStyle.Render("blank clears · today, tomorrow, +3d, +2w\nctrl+o save · esc cancel · tab next")
}
