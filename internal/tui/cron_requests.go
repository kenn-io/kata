package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

type cronRuntimeRequest struct {
	conn, scope, seq                    uint64
	target, expected, name, kind, actor string
	revision                            int64
	requestUID                          string
	inflight                            bool
	err                                 error
}
type cronRuntimeDoneMsg struct {
	request cronRuntimeRequest
	err     error
}

func (m Model) cronRuntimeTarget(kind string, lines []cronLine, cursor int) (cronRuntimeRequest, bool) {
	if m.view != viewCron || m.scope.allProjects || m.scope.projectID == 0 || m.cron.data == nil || m.cron.help || m.cron.filtering || m.cron.form != nil {
		return cronRuntimeRequest{}, false
	}
	data := m.cron.data
	ref := cronRef{}
	if n := len(m.cron.stack); n > 0 {
		ref = m.cron.stack[n-1].ref
	} else if cursor >= 0 && cursor < len(lines) {
		ref = lines[cursor].target
	}
	request := cronRuntimeRequest{conn: m.connGen, scope: m.scopeGen, target: ref.uid, kind: kind, actor: m.list.actor}
	if kind == db.CronRequestKindRun && ref.kind == cronJobs {
		job := data.job(ref.uid)
		if job == nil {
			return request, false
		}
		request.expected, request.name = job.DefinitionEventUID, job.Name
		return request, true
	}
	if kind == db.CronRequestKindCancel && ref.kind == cronRuns {
		for _, run := range data.runs {
			if run.UID == ref.uid {
				request.revision, request.name = run.Revision, run.UID
				return request, true
			}
		}
	}
	return request, false
}
func (m Model) cronRuntimeEligible(request cronRuntimeRequest) bool {
	data := m.cron.data
	if data == nil || data.projectErr != nil {
		return false
	}
	if request.kind == db.CronRequestKindRun {
		job := data.job(request.target)
		return data.jobsErr == nil && slices.Contains(data.project.controls, "run_request") && data.project.pause == nil && job != nil && job.DeletedAt == nil && job.Definition.Enabled && job.Definition.Action.Kind == "execute" && job.DefinitionEventUID == request.expected
	}
	if data.runsErr != nil || !slices.Contains(data.project.controls, "cancel_request") {
		return false
	}
	for _, run := range data.runs {
		if run.UID == request.target {
			return run.Status == "running" && run.Revision == request.revision
		}
	}
	return false
}
func (m Model) routeCronRuntimeKey(kind string, lines []cronLine, cursor int) (Model, tea.Cmd) {
	if m.cron.request != nil || m.cron.enabled != nil || m.modal != modalNone {
		return m, nil
	}
	request, ok := m.cronRuntimeTarget(kind, lines, cursor)
	if !ok || !m.cronRuntimeEligible(request) {
		return m, nil
	}
	if next, cmd, blocked := m.blockMutation(false); blocked {
		return next, cmd
	}
	m.cron.requestSeq++
	request.seq = m.cron.requestSeq
	m.cron.request = &request
	m.modal = modalCronRequest
	return m, nil
}
func (m Model) sameCronRuntimeRequest(request cronRuntimeRequest) bool {
	pending := m.cron.request
	return pending != nil && request.conn == m.connGen && request.scope == m.scopeGen && pending.seq == request.seq && pending.target == request.target && pending.kind == request.kind
}
func (m Model) confirmCronRuntimeRequest() (Model, tea.Cmd) {
	pending := m.cron.request
	if pending == nil || pending.inflight {
		return m, nil
	}
	if !m.sameCronRuntimeRequest(*pending) || m.cron.enabled != nil {
		m.modal, m.cron.request = modalNone, nil
		return m, nil
	}
	// A possibly delivered request retries its immutable identity even after the
	// selected row leaves the list; root replay precedes eligibility checks.
	if pending.requestUID == "" {
		_, _, lines := cronScreen(m)
		selected, ok := m.cronRuntimeTarget(pending.kind, lines, *m.cronCursor())
		if !ok || selected.target != pending.target || !m.cronRuntimeEligible(*pending) {
			m.modal, m.cron.request = modalNone, nil
			return m, nil
		}
	}
	if next, cmd, blocked := m.blockMutation(false); blocked {
		next.modal, next.cron.request = modalNone, nil
		return next, cmd
	}
	request := *pending
	request.inflight = true
	request.err = nil
	m.cron.request = &request
	api, pid := m.api, m.scope.projectID
	return m, func() tea.Msg {
		// Bubble Tea supplies no context; this command owns a bounded request lifetime.
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		var err error
		if request.requestUID == "" {
			if request.kind == db.CronRequestKindRun {
				current, readErr := api.ShowCronJob(ctx, pid, request.target)
				err = readErr
				if err == nil && (current.UID != request.target || current.DefinitionEventUID != request.expected || current.DeletedAt != nil || !current.Definition.Enabled || current.Definition.Action.Kind != "execute") {
					err = &APIError{Status: http.StatusConflict, Code: "cron_conflict", Message: "The job changed; refresh before retrying"}
				}
			} else {
				current, readErr := api.ShowCronRun(ctx, pid, request.target)
				err = readErr
				if err == nil && (current.UID != request.target || current.Revision != request.revision || current.Status != "running") {
					err = &APIError{Status: http.StatusConflict, Code: "cron_conflict", Message: "The run changed; refresh before retrying"}
				}
			}
			if err == nil {
				request.requestUID, err = uid.New()
			}
		}
		if err == nil {
			_, err = api.RequestCron(ctx, pid, request.requestUID, request.kind, request.target, request.actor)
		}
		return cronRuntimeDoneMsg{request, err}
	}
}
func (m Model) cronRuntimeDone(msg cronRuntimeDoneMsg) (Model, tea.Cmd) {
	if !m.sameCronRuntimeRequest(msg.request) {
		return m, nil
	}
	request := msg.request
	request.inflight = false
	request.err = msg.err
	if msg.err != nil {
		apiErr, known := errors.AsType[*APIError](msg.err)
		if request.requestUID != "" && (!known || apiErr.Status >= 500) {
			// Keep an ambiguously delivered UID for the explicit retry in this dialog.
			m.cron.request = &request
			m.modal = modalCronRequest
			return m, nil
		}
		m.cron.request = nil
		m.modal = modalNone
		m.cron.events++
		return m.undoNotice("Could not submit request: "+msg.err.Error(), toastError)
	}
	m.cron.request = nil
	m.modal = modalNone
	m.cron.events++
	m.cache.markStale()
	verb := "Requested run for "
	if request.kind == db.CronRequestKindCancel {
		verb = "Requested cancellation for "
	}
	return m.undoNotice(verb+request.name, toastSuccess)
}
func renderCronRuntimeModal(request *cronRuntimeRequest) string {
	if request == nil {
		return ""
	}
	title := "Request run for "
	explanation := "An optional runtime may run this job now."
	if request.kind == db.CronRequestKindCancel {
		title = "Request cancellation for "
		explanation = "An optional runtime may cancel this run."
	}
	lines := []string{title + truncate(sanitizeForLine(request.name), 36) + "?", "", explanation, "The request itself changes no running process."}
	if request.inflight {
		lines = append(lines, "", "Sending request…")
	} else if request.err != nil {
		lines = append(lines, "", "Response unavailable. Retry with the same UID:", request.requestUID, truncate(sanitizeForLine(request.err.Error()), 48))
	}
	return modalBoxStyle.Render(strings.Join(lines, "\n"))
}
func cronRequestDetails(d *cronDoc, data cronData, ref cronRef) {
	if !slices.Contains(data.project.controls, "run_request") && !slices.Contains(data.project.controls, "cancel_request") {
		return
	}
	if data.requestsErr != nil {
		d.section("Requests · unavailable")
		return
	}
	var requests []db.CronRequest
	for _, request := range data.requests {
		if (ref.kind == cronJobs && request.JobUID == ref.uid) || (ref.kind == cronRuns && request.RunUID != nil && *request.RunUID == ref.uid) {
			requests = append(requests, request)
		}
	}
	d.section(fmt.Sprintf("Requests (%d)", len(requests)))
	for _, request := range requests {
		d.text(request.Kind+" · "+request.State+" · "+request.UID, cronRef{})
		d.field("Requested", request.RequestedBy+" · "+request.RequestedAt.Format("2006-01-02 15:04:05Z07:00"))
		d.field("Expires", request.ExpiresAt.Format("2006-01-02 15:04:05Z07:00"))
	}
}
