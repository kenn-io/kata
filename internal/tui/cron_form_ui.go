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
	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kit/tui/helplayout"
)

// cronConfirm is a pending archive or restore awaiting y.
type cronConfirm struct {
	restore            bool
	uid, name, eventID string
}

// cronJobsScope says the keys that edit jobs apply: the Jobs list or a job's page.
func (m Model) cronJobsScope() bool {
	if m.cron.tab != cronJobs || m.scope.allProjects || m.scope.projectID == 0 {
		return false
	}
	n := len(m.cron.stack)
	return n == 0 || m.cron.stack[n-1].ref.kind == cronJobs
}

// cronSelectedJob is the job under the cursor on the list, or the open job page.
func (m Model) cronSelectedJob(lines []cronLine, cursor int) (uid string, ok bool) {
	if n := len(m.cron.stack); n > 0 {
		return m.cron.stack[n-1].ref.uid, true
	}
	if cursor < 0 || cursor >= len(lines) || lines[cursor].target.uid == "" {
		return "", false
	}
	return lines[cursor].target.uid, true
}

// jobAny finds the loaded job, live or archived.
func (p cronData) jobAny(uid string) *generated.CronJob {
	if j := p.job(uid); j != nil {
		return j
	}
	for _, j := range p.archived {
		if j.UID == uid {
			return &j
		}
	}
	return nil
}

func (m Model) cronToast(text string, level toastLevel) (Model, tea.Cmd) {
	m.toast = &toast{text: text, level: level, expiresAt: m.toastNow().Add(3 * time.Second)}
	return m, toastExpireCmd(3 * time.Second)
}

// routeCronJobKey handles the job keys. It reports false for any other key.
func (m Model) routeCronJobKey(key string, lines []cronLine, cursor int) (Model, tea.Cmd, bool) {
	if !m.cronJobsScope() || m.cron.data == nil {
		return m, nil, false
	}
	switch key {
	case "n":
		if m.cron.archived && len(m.cron.stack) == 0 {
			return m, nil, false
		}
		f, err := newCronForm(m.cronWorkflowChoices())
		if err != nil {
			m, cmd := m.cronToast("Could not start a job: "+err.Error(), toastError)
			return m, cmd, true
		}
		m.cron.form = f
		return m, nil, true
	case "a":
		if len(m.cron.stack) > 0 {
			return m, nil, false
		}
		m.cron.archived = !m.cron.archived
		m.cron.lists[cronJobs].cursor = 0
		m.cron.events++
		return m, nil, true
	case "e", "p", "d", "u":
	default:
		return m, nil, false
	}
	uid, ok := m.cronSelectedJob(lines, cursor)
	job := m.cron.data.jobAny(uid)
	if !ok || job == nil {
		return m, nil, true
	}
	archived := job.DeletedAt != nil
	switch {
	case key == "u" && !archived, key == "d" && archived:
		return m, nil, true
	case (key == "e" || key == "p") && archived:
		m, cmd := m.cronToast("The job is archived; restore it with u first", toastInfo)
		return m, cmd, true
	case key == "d" || key == "u":
		m.cron.confirm = &cronConfirm{restore: key == "u", uid: uid, name: job.Name, eventID: job.DefinitionEventUID}
		return m, nil, true
	case key == "e":
		return m, m.cronOpenEdit(uid), true
	}
	return m, m.cronTogglePause(*job), true
}

// cronWorkflowChoices are the project's live workflows for the workflow field.
func (m Model) cronWorkflowChoices() []cronChoice {
	var out []cronChoice
	for _, w := range m.cron.data.workflows {
		out = append(out, cronChoice{w.UID, w.Name})
	}
	return out
}

type cronEditOpenedMsg struct {
	conn, scope uint64
	record      CronJobRecord
	err         error
}

// cronSavedMsg answers a save, pause, archive or restore. conflict carries
// the job as it is now when the guard refused the write.
type cronSavedMsg struct {
	conn, scope uint64
	notice      string
	conflict    *CronJobRecord
	fieldErr    *cronFieldError
	err         error
	fromForm    bool
}

func (m Model) cronOpenEdit(uid string) tea.Cmd {
	api, pid, conn, scope := m.api, m.scope.projectID, m.connGen, m.scopeGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		rec, err := api.ShowCronJob(ctx, pid, uid)
		return cronEditOpenedMsg{conn: conn, scope: scope, record: rec, err: err}
	}
}

func (m Model) cronEditOpened(msg cronEditOpenedMsg) (Model, tea.Cmd) {
	if msg.conn != m.connGen || msg.scope != m.scopeGen || m.cron.data == nil || m.cron.form != nil {
		return m, nil
	}
	if msg.err != nil {
		return m.cronToast("Could not open the job: "+cronWriteMessage(msg.err), toastError)
	}
	f, err := cronFormFor(msg.record, m.cronWorkflowChoices())
	if err != nil {
		return m.cronToast(err.Error(), toastError)
	}
	m.cron.form = f
	return m, nil
}

// cronTogglePause flips enabled on the definition as it is now, guarded on the
// event the user saw, so a pause never overwrites an unseen edit.
func (m Model) cronTogglePause(job generated.CronJob) tea.Cmd {
	api, pid, actor := m.api, m.scope.projectID, m.list.actor
	conn, scope := m.connGen, m.scopeGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		done := func(notice string, err error) tea.Msg {
			return cronSavedMsg{conn: conn, scope: scope, notice: notice, err: err}
		}
		rec, err := api.ShowCronJob(ctx, pid, job.UID)
		if err != nil {
			return done("", err)
		}
		if rec.DefinitionEventUID != job.DefinitionEventUID {
			return done("", errors.New("the job changed since it was loaded; refreshed, try again"))
		}
		rec.Definition.Enabled = !rec.Definition.Enabled
		_, err = api.PutCronJob(ctx, pid, job.UID, CronJobWrite{Name: rec.Name, Definition: rec.Definition, ExpectedEventUID: rec.DefinitionEventUID, Actor: actor})
		notice := "Paused " + rec.Name
		if rec.Definition.Enabled {
			notice = "Resumed " + rec.Name
		}
		return done(notice, err)
	}
}

func (m Model) cronLifecycle(c cronConfirm) tea.Cmd {
	api, pid, actor := m.api, m.scope.projectID, m.list.actor
	conn, scope := m.connGen, m.scopeGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		call, notice := api.ArchiveCronJob, "Archived "+c.name
		if c.restore {
			call, notice = api.RestoreCronJob, "Restored "+c.name
		}
		return cronSavedMsg{conn: conn, scope: scope, notice: notice, err: call(ctx, pid, c.uid, c.eventID, actor)}
	}
}

// cronSaveCmd resolves the issue refs the draft names, builds the definition
// and writes it. A refused guard reads the job as it is now so the form can
// show it beside the draft.
func (m Model) cronSaveCmd(f cronForm) tea.Cmd {
	api, pid, actor := m.api, m.scope.projectID, m.list.actor
	conn, scope := m.connGen, m.scopeGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		fail := func(err error) tea.Msg { return cronSavedMsg{conn: conn, scope: scope, err: err, fromForm: true} }
		resolved := map[string]string{}
		for _, r := range f.issueRefs() {
			detail, err := api.GetIssueDetail(ctx, pid, r.ref)
			if err != nil || detail == nil || detail.Issue == nil || detail.Issue.UID == "" {
				text := fmt.Sprintf("no issue %q in this project", r.ref)
				if err != nil && !isNotFound(err) {
					text = cronWriteMessage(err)
				}
				return cronSavedMsg{conn: conn, scope: scope, fieldErr: &cronFieldError{r.field, text}, fromForm: true}
			}
			resolved[r.ref] = detail.Issue.UID
		}
		name, def, err := f.build(cronUIDResolver(resolved))
		if err != nil {
			if fe, ok := err.(*cronFieldError); ok {
				return cronSavedMsg{conn: conn, scope: scope, fieldErr: fe, fromForm: true}
			}
			return fail(err)
		}
		write := CronJobWrite{Name: name, Definition: def, Actor: actor}
		notice := "Created " + name
		if !f.create {
			write.ExpectedEventUID, notice = f.expected, "Saved "+name
		}
		if _, err := api.PutCronJob(ctx, pid, f.uid, write); err != nil {
			var apiErr *APIError
			if !f.create && errors.As(err, &apiErr) && apiErr != nil && apiErr.Status == http.StatusConflict {
				if current, readErr := api.ShowCronJob(ctx, pid, f.uid); readErr == nil {
					return cronSavedMsg{conn: conn, scope: scope, conflict: &current, err: err, fromForm: true}
				}
			}
			return fail(err)
		}
		return cronSavedMsg{conn: conn, scope: scope, notice: notice, fromForm: true}
	}
}

func isNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr != nil && apiErr.Status == http.StatusNotFound
}

// cronWriteMessage words a failed cron request. A rejected token and a token
// that may not touch the project are different problems.
func cronWriteMessage(err error) string {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return err.Error()
	}
	switch {
	case apiErr.Status == http.StatusForbidden && apiErr.Code == "token_invalid":
		return "the daemon rejected the token as invalid or revoked; replace it and try again"
	case apiErr.Status == http.StatusForbidden:
		return "this token may not change cron jobs in this project (" + err.Error() + ")"
	case apiErr.Status == http.StatusUnauthorized:
		return "the daemon asked for authentication: " + err.Error()
	}
	return err.Error()
}

func (m Model) cronSaved(msg cronSavedMsg) (Model, tea.Cmd) {
	if msg.conn != m.connGen || msg.scope != m.scopeGen {
		return m, nil
	}
	f := m.cron.form
	if msg.fromForm && f != nil {
		f.saving = false
	}
	switch {
	case msg.conflict != nil && f != nil:
		f.conflict, f.general = msg.conflict, ""
		return m, nil
	case msg.fieldErr != nil && f != nil:
		f.err, f.general = msg.fieldErr, ""
		f.focusField(msg.fieldErr.field)
		return m, nil
	case msg.err != nil:
		m.cron.events++
		if msg.fromForm && f != nil {
			f.general = cronWriteMessage(msg.err)
			return m, nil
		}
		return m.cronToast(cronWriteMessage(msg.err), toastError)
	}
	if msg.fromForm {
		m.cron.form = nil
	}
	m.cron.events++
	return m.cronToast(msg.notice, toastSuccess)
}

func (m Model) routeCronConfirmKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	c := *m.cron.confirm
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "y", "Y":
		m.cron.confirm = nil
		return m, m.cronLifecycle(c)
	case "n", "N", "esc":
		m.cron.confirm = nil
	}
	return m, nil
}

func (m Model) routeCronFormKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	f := m.cron.form
	key := msg.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if f.saving {
		return m, nil
	}
	if f.conflict != nil {
		return m.routeCronConflictKey(key)
	}
	switch key {
	case "esc":
		if f.dirty() {
			m.modal = modalDiscardCronJob
			return m, nil
		}
		m.cron.form = nil
	case "ctrl+o", "ctrl+s":
		f.validate()
		if f.err != nil || f.general != "" {
			if f.err != nil {
				f.focusField(f.err.field)
			}
			return m, nil
		}
		f.saving = true
		return m, m.cronSaveCmd(*f)
	case "tab", "down", "enter":
		f.move(1)
	case "shift+tab", "up":
		f.move(-1)
	case "left", "right", "space":
		if f.isChoice(f.focus) {
			delta := 1
			if key == "left" {
				delta = -1
			}
			f.step(f.focus, delta)
			break
		}
		fallthrough
	default:
		if f.isChoice(f.focus) || f.locked(f.focus) {
			break
		}
		f.ed, _ = f.ed.Update(msg)
		f.validate()
		f.general = ""
	}
	return m, nil
}

// routeCronConflictKey answers the newer job a save found: apply the draft to
// it, take the newer one instead, or go back to the draft.
func (m Model) routeCronConflictKey(key string) (Model, tea.Cmd) {
	f := m.cron.form
	current := *f.conflict
	switch key {
	case "a":
		if current.Archived {
			return m, nil
		}
		next, err := f.rebase(current)
		if err != nil {
			f.general = err.Error()
			return m, nil
		}
		m.cron.form = next
		if next.err != nil || next.general != "" {
			return m, nil
		}
		next.saving = true
		return m, m.cronSaveCmd(*next)
	case "t":
		next, err := cronFormFor(current, f.workflows)
		if err != nil {
			f.general = err.Error()
			return m, nil
		}
		m.cron.form = next
	case "esc":
		f.conflict = nil
	}
	return m, nil
}

func cronFormHelpRows(f *cronForm) []helplayout.HelpItem {
	switch {
	case f.conflict != nil:
		rows := []helplayout.HelpItem{{Key: "t", Description: "take the current job"}, {Key: "esc", Description: "back to my draft"}}
		if !f.conflict.Archived {
			rows = slices.Insert(rows, 0, helplayout.HelpItem{Key: "a", Description: "apply my draft to it"})
		}
		return rows
	}
	return []helplayout.HelpItem{
		{Key: "tab/↑↓", Description: "field"},
		{Key: "←/→", Description: "choose"},
		{Key: "ctrl+o", Description: "save"},
		{Key: "esc", Description: "cancel"},
	}
}

func cronFieldLabel(f *cronForm, field cronField) string {
	switch field {
	case cfIssue:
		switch {
		case cronIsDate(f.trigger):
			return "Issue whose date fires"
		case f.kind == cronNotifyKind.id:
			return "Notify about issue"
		}
		return "Run issue"
	case cfReport:
		if f.kind == cronNotifyKind.id {
			return "Notify about issue"
		}
		return "Run issue"
	}
	return cronFieldLabels[field]
}

// cronFormLines renders the form body, one line per field.
func cronFormLines(m Model, f *cronForm) (lines []string, cursor int) {
	lines = make([]string, 0, cfCount)
	if f.conflict != nil {
		c := f.conflict
		lines = append(lines, errorStyle.Render("This job changed while you edited it. Your draft is kept."), "",
			"Current name:       "+sanitizeForLine(c.Name),
			"Current state:      "+map[bool]string{true: "enabled", false: "paused"}[c.Definition.Enabled]+map[bool]string{true: " (archived)", false: ""}[c.Archived],
			"Current trigger:    "+sanitizeForLine(cronCurrentTrigger(c.Definition.Trigger)),
			"Current action:     "+sanitizeForLine(c.Definition.Action.Kind),
			"Current event:      "+c.DefinitionEventUID)
		return lines, 0
	}
	labelWidth := 0
	order := f.order()
	for _, field := range order {
		labelWidth = max(labelWidth, len([]rune(cronFieldLabel(f, field))))
	}
	for i, field := range order {
		if field == f.focus {
			cursor = i
		}
		prefix := "  "
		if field == f.focus {
			prefix = "▶ "
		}
		var value string
		switch {
		case f.isChoice(field):
			value = "‹ " + f.display(field) + " ›"
		case field == f.focus && !f.locked(field):
			value = f.ed.View()
		default:
			value = sanitizeForLine(strings.ReplaceAll(f.val[field], "\n", " ⏎ "))
			if value == "" && cronFieldHints[field] != "" {
				value = statsLineStyle.Render(cronFieldHints[field])
			}
		}
		if f.locked(field) {
			value += "  (locked)"
		}
		line := fmt.Sprintf("%s%-*s  %s", prefix, labelWidth+1, cronFieldLabel(f, field)+":", value)
		if f.err != nil && f.err.field == field {
			line += "  " + errorStyle.Render(sanitizeForLine(f.err.text))
		}
		lines = append(lines, ansi.Truncate(line, m.width, "…"))
	}
	return lines, cursor
}

func cronCurrentTrigger(t cron.Trigger) string {
	return cronSchedule(generated.TriggerResponse{Kind: t.Kind, Cron: &t.Cron, IntervalSeconds: &t.IntervalSeconds, At: &t.At, Timezone: &t.Timezone})
}

func renderCronForm(m Model) string {
	f := m.cron.form
	title := "New job"
	if !f.create {
		title = "Edit job · " + sanitizeForLine(f.openedName)
	}
	body := []string{
		renderTitleBar(m.width, m.scope, kataVersion, m.chrome().daemon),
		cronTabs(m.cron.tab),
		statsLineStyle.Render(padLeftRightInside(title, f.uid, titleBarInnerWidth(m.width))),
		"",
	}
	help := modalFirstHelpRows(m.modal, [][]helplayout.HelpItem{cronFormHelpRows(f)})
	lines, cursor := cronFormLines(m, f)
	footer := []string{}
	switch {
	case f.saving:
		footer = append(footer, "saving…")
	case f.general != "":
		footer = append(footer, errorStyle.Render(truncate(sanitizeForLine(f.general), m.width)))
	}
	budget := max(m.height-len(body)-max(len(footer), 1)-helpLines(help, m.width)-cronNoticeRows(m), 1)
	start, end := windowBounds(len(lines), cursor, budget)
	body = append(body, lines[start:end]...)
	for range max(budget-(end-start), 0) {
		body = append(body, "")
	}
	if len(footer) == 0 {
		footer = append(footer, "")
	}
	body = append(body, footer...)
	body = append(body, renderFooterHelpTable(help, m.width))
	return strings.Join(body, "\n")
}
