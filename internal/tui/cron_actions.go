package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

const (
	cronPause  = "pause"
	cronResume = "resume"
)

// cronPending is the one pending cron mutation: p pauses or resumes a job, R
// requests a run (db.CronRequestKindRun), and X requests a cancellation
// (db.CronRequestKindCancel). Connection, scope, and sequence fence it.
type cronPending struct {
	conn, scope, seq                    uint64
	kind, target, expected, name, actor string
	parent                              string // resume: the parent whose progress was counted
	count                               int    // resume: missed children at lookup
	allowDiscard, discard, invalidated  bool
	revision                            int64 // cancel: the running observation admitted
	requestUID                          string
	inflight                            bool
	err                                 error // request: the ambiguous outcome offered for retry
}

func (a cronPending) request() bool {
	return a.kind == db.CronRequestKindRun || a.kind == db.CronRequestKindCancel
}

// cronActionMsg answers a resume's backlog lookup (or a pause, which needs
// none), or with done set, the write itself.
type cronActionMsg struct {
	action cronPending
	err    error
	done   bool
}

// cronSelection is the open page, or the row under the cursor, while the cron
// screen is in plain navigation of one project.
func (m Model) cronSelection() (cronRef, bool) {
	if m.view != viewCron || m.cron.help || m.cron.filtering || m.cron.form != nil || m.cron.data == nil || m.scope.allProjects || m.scope.projectID == 0 {
		return cronRef{}, false
	}
	if n := len(m.cron.stack); n > 0 {
		return m.cron.stack[n-1].ref, true
	}
	_, _, lines := cronScreen(m)
	if cursor := *m.cronCursor(); cursor >= 0 && cursor < len(lines) {
		return lines[cursor].target, true
	}
	return cronRef{}, false
}

// cronActionCurrent reports whether the action's target is still selected and
// still eligible in the loaded data.
func (m Model) cronActionCurrent(a cronPending) bool {
	ref, ok := m.cronSelection()
	if !ok || ref.uid != a.target {
		return false
	}
	data := m.cron.data
	job := data.job(a.target)
	switch a.kind {
	case cronPause, cronResume:
		return m.cronJobsScope() && data.jobsErr == nil && job != nil && job.DeletedAt == nil && job.DefinitionEventUID == a.expected
	case db.CronRequestKindRun:
		return ref.kind == cronJobs && data.projectErr == nil && data.jobsErr == nil && slices.Contains(data.project.controls, "run_request") && data.project.pause == nil &&
			job != nil && job.DeletedAt == nil && job.Definition.Enabled && job.Definition.Action.Kind == "execute" && job.DefinitionEventUID == a.expected
	}
	if ref.kind != cronRuns || data.projectErr != nil || data.runsErr != nil || !slices.Contains(data.project.controls, "cancel_request") {
		return false
	}
	for _, run := range data.runs {
		if run.UID == a.target {
			return run.Status == "running" && run.Revision == a.revision
		}
	}
	return false
}

// selectedCronAction is the selected row's action of this kind, if eligible.
func (m Model) selectedCronAction(kind string) (cronPending, bool) {
	ref, _ := m.cronSelection()
	a := cronPending{kind: kind, target: ref.uid, name: ref.uid}
	if job := m.cron.data.job(ref.uid); job != nil {
		a.expected, a.name, a.parent = job.DefinitionEventUID, job.Name, cronNativeParent(*job)
		if kind == cronPause && !job.Definition.Enabled {
			a.kind = cronResume
		}
	}
	for _, run := range m.cron.data.runs {
		if run.UID == ref.uid {
			a.revision = run.Revision
		}
	}
	return a, m.cronActionCurrent(a)
}

func (m Model) startCronAction(a cronPending) (Model, cronPending) {
	m.cron.actionSeq++
	a.conn, a.scope, a.seq, a.actor = m.connGen, m.scopeGen, m.cron.actionSeq, m.list.actor
	m.cron.action = &a
	return m, a
}

func (m Model) sameCronAction(a cronPending) bool {
	pending := m.cron.action
	return pending != nil && a.conn == m.connGen && a.scope == m.scopeGen && pending.seq == a.seq
}

// dropCronAction forgets the pending action and closes its dialog.
func (m Model) dropCronAction() Model {
	if m.modal == modalCronAction {
		m.modal = modalNone
	}
	m.cron.action = nil
	return m
}

// routeCronEnabledKey handles p. Pause writes at once; resume first counts the
// backlog it would deliver.
func (m Model) routeCronEnabledKey() (Model, tea.Cmd) {
	if !m.cronJobsScope() || m.cron.data == nil || m.cron.action != nil || m.cron.data.jobsErr != nil {
		return m, nil
	}
	if next, cmd, blocked := m.blockMutation(false); blocked {
		return next, cmd
	}
	a, ok := m.selectedCronAction(cronPause)
	if !ok {
		return m, nil
	}
	m, a = m.startCronAction(a)
	m.cron.editSeq++
	api, pid := m.api, m.scope.projectID
	return m, func() tea.Msg {
		if a.kind == cronPause {
			return cronActionMsg{action: a}
		}
		// Bubble Tea commands own their request lifetime; the framework supplies no context.
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		var err error
		a.count, err = cronResumeBacklog(ctx, api, pid, a)
		return cronActionMsg{action: a, err: err}
	}
}

// cronResumeBacklog counts the selected job's children that came due while it
// was paused, under the revision the user selected.
func cronResumeBacklog(ctx context.Context, api cronAPI, pid int64, a cronPending) (int, error) {
	read, err := api.ShowCronJob(ctx, pid, a.target)
	if err != nil {
		return 0, err
	}
	if read.Passover != nil && read.Passover.Invalid {
		return 0, errors.New("invalid cutoff; repair with --discard-missed")
	}
	job := read.Job
	if job.DeletedAt != nil || job.DefinitionEventUID != a.expected {
		return 0, cronChanged("job")
	}
	if !cron.IsRecurringIssueJob(job.Definition) {
		return 0, nil
	}
	parent, err := api.readCronParent(ctx, pid, job.Definition.Issue.ParentUID)
	if err != nil || !parent.open {
		return 0, err
	}
	progress, ok := parent.progress[job.UID]
	if !ok {
		return 0, errors.New("cannot count missed runs; refresh the job before resuming")
	}
	if read.Passover != nil {
		if progress, err = cron.ApplyPassover(progress, read.Passover.Through); err != nil {
			return 0, err
		}
	}
	job.Definition.Enabled = true
	return cron.RecurringBacklog(ctx, job.Definition, a.expected, progress, time.Now().UTC())
}

func cronChanged(what string) error {
	return &APIError{Status: http.StatusConflict, Code: "cron_conflict", Message: "The " + what + " changed; refresh before retrying"}
}

// routeCronRuntimeKey opens the R or X confirmation for an eligible row.
func (m Model) routeCronRuntimeKey(kind string) (Model, tea.Cmd) {
	if m.cron.action != nil || m.cron.data == nil {
		return m, nil
	}
	a, ok := m.selectedCronAction(kind)
	if !ok {
		return m, nil
	}
	if next, cmd, blocked := m.blockMutation(false); blocked {
		return next, cmd
	}
	m, _ = m.startCronAction(a)
	m.modal = modalCronAction
	return m, nil
}

// recheckCronAction drops an action whose job data changed, whose target is no
// longer selected and eligible, or that the connection may not write. A request
// retrying a possibly delivered UID keeps it even after its row leaves the list.
func (m Model) recheckCronAction(a cronPending) (Model, tea.Cmd, bool) {
	if m.cron.action.invalidated {
		next, cmd := m.dropCronAction().undoNotice("Job data changed; press p after refresh", toastError)
		return next, cmd, false
	}
	if a.requestUID == "" && !m.cronActionCurrent(a) {
		return m.dropCronAction(), nil, false
	}
	if next, cmd, blocked := m.blockMutation(false); blocked {
		return next.dropCronAction(), cmd, false
	}
	return m, nil, true
}

// cronActionAnswered asks to confirm a counted backlog, writes at once when
// there is none, or finishes a write.
func (m Model) cronActionAnswered(msg cronActionMsg) (Model, tea.Cmd) {
	if !m.sameCronAction(msg.action) {
		return m, nil
	}
	if msg.done {
		return m.cronActionDone(msg.action, msg.err)
	}
	if m.modal != modalNone {
		return m.dropCronAction(), nil
	}
	if next, cmd, ok := m.recheckCronAction(msg.action); !ok {
		return next, cmd
	}
	if msg.err != nil {
		return m.cronActionDone(msg.action, msg.err)
	}
	a := msg.action
	a.allowDiscard = m.cron.data.projectErr == nil && slices.Contains(m.cron.data.project.controls, "resume_discard_missed")
	m.cron.action = &a
	if a.count > 0 {
		m.modal = modalCronAction
		return m, nil
	}
	return m, m.sendCronAction(a)
}

// confirmCronAction answers the dialog with y, or with d to resume discarding
// the backlog.
func (m Model) confirmCronAction(discard bool) (Model, tea.Cmd) {
	a := m.cron.action
	if a.inflight {
		return m, nil
	}
	if !m.sameCronAction(*a) {
		return m.dropCronAction(), nil
	}
	if next, cmd, ok := m.recheckCronAction(*a); !ok {
		return next, cmd
	}
	sent := *a
	sent.discard = discard
	if sent.request() {
		// The dialog stays open, refusing other keys, until the outcome arrives.
		sent.inflight, sent.err = true, nil
		m.cron.action = &sent
	} else {
		m.modal = modalNone
	}
	return m, m.sendCronAction(sent)
}

func (m Model) sendCronAction(a cronPending) tea.Cmd {
	api, pid := m.api, m.scope.projectID
	timeout := defaultHTTPTimeout
	if !a.request() {
		timeout = cronEnabledTimeout
	}
	return func() tea.Msg {
		// Bubble Tea supplies no context; bound the independent command lifetime.
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if !a.request() {
			_, err := api.CronJobEnabled(ctx, pid, a.target, a.expected, a.actor, a.kind == cronResume, a.discard)
			return cronActionMsg{action: a, err: err, done: true}
		}
		var err error
		if a.requestUID == "" {
			// The UID is generated only after the live target still matches.
			if err = cronRequestTargetLive(ctx, api, pid, a); err == nil {
				a.requestUID, err = uid.New()
			}
		}
		if err == nil {
			err = api.RequestCron(ctx, pid, a.requestUID, a.kind, a.target, a.actor)
		}
		return cronActionMsg{action: a, err: err, done: true}
	}
}

func cronRequestTargetLive(ctx context.Context, api cronAPI, pid int64, a cronPending) error {
	if a.kind == db.CronRequestKindRun {
		read, err := api.ShowCronJob(ctx, pid, a.target)
		if err == nil && (read.Job.DefinitionEventUID != a.expected || read.Job.DeletedAt != nil || !read.Job.Definition.Enabled || read.Job.Definition.Action.Kind != "execute") {
			err = cronChanged("job")
		}
		return err
	}
	run, err := api.ShowCronRun(ctx, pid, a.target)
	if err == nil && (run.Revision != a.revision || run.Status != "running") {
		err = cronChanged("run")
	}
	return err
}

// cronActionDone ends the action, refreshes cron data, and reports the
// outcome. A request keeps an ambiguously delivered UID for explicit retry.
func (m Model) cronActionDone(a cronPending, err error) (Model, tea.Cmd) {
	if apiErr, known := errors.AsType[*APIError](err); err != nil && a.requestUID != "" && (!known || apiErr.Status >= 500) {
		a.inflight, a.err = false, err
		m.cron.action = &a
		return m, nil
	}
	m = m.dropCronAction()
	m.cron.events++
	m.cache.markStale()
	if err != nil {
		failure := "Could not change the job: "
		if a.request() {
			failure = "Could not submit request: "
		}
		return m.undoNotice(failure+err.Error(), toastError)
	}
	verb := map[string]string{cronPause: "Paused ", cronResume: "Resumed ", db.CronRequestKindRun: "Requested run for ", db.CronRequestKindCancel: "Requested cancellation for "}[a.kind]
	return m.undoNotice(verb+a.name, toastSuccess)
}

func renderCronActionModal(a *cronPending) string {
	var lines []string
	switch {
	case a.request():
		title, explanation := "Request run for ", "An optional runtime may run this job now."
		if a.kind == db.CronRequestKindCancel {
			title, explanation = "Request cancellation for ", "An optional runtime may cancel this run."
		}
		lines = []string{title + truncate(sanitizeForLine(a.name), 36) + "?", "", explanation, "The request itself changes no running process."}
		if a.inflight {
			lines = append(lines, "", "Sending request…")
		} else if a.err != nil {
			lines = append(lines, "", "Response unavailable. Retry with the same UID:", a.requestUID, truncate(sanitizeForLine(a.err.Error()), 48))
		}
	default:
		issues := "issues"
		if a.count == 1 {
			issues = "issue"
		}
		lines = []string{"Resume " + sanitizeForLine(a.name) + "?", "", fmt.Sprintf("Backlog at last check: %d recurring child", a.count), issues + " that came due while it was paused.", ""}
		if a.allowDiscard {
			lines = append(lines, "y resumes with the existing catch-up policy.", "d resumes and discards all missed runs up to", "when it is saved, including older backlog.")
		} else {
			lines = append(lines, "To change how missed runs are handled,", "use `kata cron job update`.")
		}
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
