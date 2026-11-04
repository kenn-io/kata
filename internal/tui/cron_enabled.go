package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/cron"
)

type cronEnabledRequest struct {
	conn, scope, seq            uint64
	uid, expected, name, parent string
	enabled, invalidated        bool
	count                       int
}
type cronEnabledReadyMsg struct {
	request cronEnabledRequest
	err     error
}
type cronEnabledDoneMsg struct {
	request cronEnabledRequest
	err     error
}

func (m Model) routeCronEnabledKey(lines []cronLine, cursor int) (Model, tea.Cmd) {
	if !m.cronJobsScope() || m.cron.data == nil || m.cron.enabled != nil || m.cron.data.jobsErr != nil {
		return m, nil
	}
	if next, cmd, blocked := m.blockMutation(false); blocked {
		return next, cmd
	}
	id, ok := m.cronSelectedJob(lines, cursor)
	job := m.cron.data.job(id)
	if !ok || job == nil || job.DeletedAt != nil {
		return m, nil
	}
	m.cron.enabledSeq++
	m.cron.editSeq++
	request := cronEnabledRequest{conn: m.connGen, scope: m.scopeGen, seq: m.cron.enabledSeq, parent: cronNativeParent(*job), uid: id, expected: job.DefinitionEventUID, name: job.Name, enabled: !job.Definition.Enabled}
	m.cron.enabled = &request
	api, pid := m.api, m.scope.projectID
	return m, func() tea.Msg {
		if !request.enabled {
			return cronEnabledReadyMsg{request: request}
		}
		// Bubble Tea commands own their request lifetime; the framework supplies no context.
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		current, err := api.ShowCronJob(ctx, pid, request.uid)
		if err == nil && (current.DeletedAt != nil || current.DefinitionEventUID != request.expected) {
			err = &APIError{Status: http.StatusConflict, Code: "cron_conflict", Message: "The job changed; refresh before retrying"}
		}
		if err == nil && cron.IsRecurringIssueJob(current.Definition) {
			parent, readErr := api.readCronParent(ctx, pid, current.Definition.Issue.ParentUID)
			err = readErr
			if err == nil && parent.open {
				progress, ok := parent.progress[current.UID]
				if !ok {
					err = errors.New("cannot count missed runs; refresh the job before resuming")
				} else {
					definition := current.Definition
					definition.Enabled = true
					request.count, err = cron.RecurringBacklog(ctx, definition, request.expected, progress, time.Now().UTC())
				}
			}
		}
		return cronEnabledReadyMsg{request, err}
	}
}

func (m Model) sameCronEnabledRequest(request cronEnabledRequest) bool {
	pending := m.cron.enabled
	return pending != nil && request.conn == m.connGen && request.scope == m.scopeGen && pending.seq == request.seq && pending.uid == request.uid && pending.expected == request.expected
}
func (m Model) cronEnabledSelection(request cronEnabledRequest) bool {
	if m.view != viewCron || m.cron.help || m.cron.filtering || !m.cronJobsScope() || m.cron.data == nil || m.cron.form != nil || m.cron.data.jobsErr != nil {
		return false
	}
	_, _, lines := cronScreen(m)
	id, ok := m.cronSelectedJob(lines, *m.cronCursor())
	job := m.cron.data.job(id)
	return ok && id == request.uid && job != nil && job.DeletedAt == nil && job.DefinitionEventUID == request.expected
}
func (m Model) cronEnabledReady(msg cronEnabledReadyMsg) (Model, tea.Cmd) {
	if !m.sameCronEnabledRequest(msg.request) {
		return m, nil
	}
	if m.modal == modalNone && m.cron.enabled.invalidated {
		m.cron.enabled = nil
		return m.undoNotice("Job data changed; press p after refresh", toastError)
	}
	if !m.cronEnabledSelection(msg.request) || m.modal != modalNone {
		m.cron.enabled = nil
		return m, nil
	}
	if next, cmd, blocked := m.blockMutation(false); blocked {
		next.cron.enabled = nil
		return next, cmd
	}
	if msg.err != nil {
		m.cron.enabled = nil
		m.cron.events++
		return m.undoNotice("Could not change the job: "+msg.err.Error(), toastError)
	}
	request := msg.request
	m.cron.enabled = &request
	if request.count > 0 {
		m.modal = modalCronResume
		return m, nil
	}
	return m, m.cronEnabledWrite(request)
}
func (m Model) cronEnabledWrite(request cronEnabledRequest) tea.Cmd {
	api, pid, actor := m.api, m.scope.projectID, m.list.actor
	return func() tea.Msg {
		// Bubble Tea supplies no context; bound the independent command lifetime.
		ctx, cancel := context.WithTimeout(context.Background(), cronEnabledTimeout)
		defer cancel()
		_, err := api.CronJobEnabled(ctx, pid, request.uid, request.expected, actor, request.enabled)
		return cronEnabledDoneMsg{request, err}
	}
}
func (m Model) confirmCronResume() (Model, tea.Cmd) {
	m.modal = modalNone
	pending := m.cron.enabled
	if pending == nil {
		return m, nil
	}
	if m.sameCronEnabledRequest(*pending) && pending.invalidated {
		m.cron.enabled = nil
		return m.undoNotice("Job data changed; press p after refresh", toastError)
	}
	if !m.sameCronEnabledRequest(*pending) || !m.cronEnabledSelection(*pending) {
		m.cron.enabled = nil
		return m, nil
	}
	if next, cmd, blocked := m.blockMutation(false); blocked {
		next.cron.enabled = nil
		return next, cmd
	}
	return m, m.cronEnabledWrite(*pending)
}
func (m Model) cronEnabledDone(msg cronEnabledDoneMsg) (Model, tea.Cmd) {
	if !m.sameCronEnabledRequest(msg.request) {
		return m, nil
	}
	m.cron.enabled = nil
	m.cron.events++
	m.cache.markStale()
	if msg.err != nil {
		return m.undoNotice("Could not change the job: "+msg.err.Error(), toastError)
	}
	verb := "Paused "
	if msg.request.enabled {
		verb = "Resumed "
	}
	return m.undoNotice(verb+msg.request.name, toastSuccess)
}
func renderCronResumeModal(request *cronEnabledRequest) string {
	if request == nil {
		return ""
	}
	issues := "issues"
	if request.count == 1 {
		issues = "issue"
	}
	return modalBoxStyle.Render(strings.Join([]string{
		"Resume " + sanitizeForLine(request.name) + "?", "",
		fmt.Sprintf("Backlog at last check: %d recurring child", request.count),
		issues + " that came due while it was paused.", "",
		"To change how missed runs are handled,",
		"use `kata cron job update`.",
	}, "\n"))
}
