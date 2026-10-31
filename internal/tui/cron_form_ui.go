package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/termtext"
)

// cronJobsScope says the keys that edit jobs apply: the Jobs list or a job's page.
func (m Model) cronJobsScope() bool {
	if m.scope.allProjects || m.scope.projectID == 0 {
		return false
	}
	if n := len(m.cron.stack); n > 0 {
		return m.cron.stack[n-1].ref.kind == cronJobs
	}
	return m.cron.tab == cronJobs
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
		f, err := newCronForm(m.cronWorkflowChoices())
		if err != nil {
			m, cmd := m.cronToast("Could not start a job: "+err.Error(), toastError)
			return m, cmd, true
		}
		m.cron.form = f
		return m, nil, true
	case "e":
	default:
		return m, nil, false
	}
	uid, ok := m.cronSelectedJob(lines, cursor)
	job := m.cron.data.job(uid)
	if !ok || job == nil {
		return m, nil, true
	}

	m.cron.editSeq++
	return m, m.cronOpenEdit(uid, m.cron.editSeq), true
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
	conn, scope, seq uint64
	uid              string
	record           CronJobRecord
	err              error
}

// cronSavedMsg answers a save. conflict carries
// the job as it is now when the guard refused the write.
type cronSavedMsg struct {
	conn, scope uint64
	notice      string
	conflict    *CronJobRecord
	fieldErr    *cronFieldError
	err         error
	fromForm    bool
}

func (m Model) cronOpenEdit(uid string, seq uint64) tea.Cmd {
	api, pid, conn, scope := m.api, m.scope.projectID, m.connGen, m.scopeGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		rec, err := api.ShowCronJob(ctx, pid, uid)
		return cronEditOpenedMsg{conn: conn, scope: scope, seq: seq, uid: uid, record: rec, err: err}
	}
}

func (m Model) cronEditOpened(msg cronEditOpenedMsg) (Model, tea.Cmd) {
	if msg.conn != m.connGen || msg.scope != m.scopeGen || m.cron.data == nil || m.cron.form != nil || msg.seq != m.cron.editSeq || m.cron.filtering || m.modal != modalNone {
		return m, nil
	}
	// The answer may arrive after the user moved on; it opens the form only
	// where the key was pressed: the same job, with nothing else on top.
	_, _, lines := cronScreen(m)
	if uid, ok := m.cronSelectedJob(lines, *m.cronCursor()); m.view != viewCron || m.cron.help || !m.cronJobsScope() || !ok || uid != msg.uid {
		return m, nil
	}
	if msg.err != nil {
		return m.cronToast("Could not open the job: "+cronWriteMessage(msg.err), toastError)
	}
	if msg.record.Archived {
		return m.cronToast("The job was archived; use the CLI to restore it before editing", toastInfo)
	}
	f, err := cronFormFor(msg.record, m.cronWorkflowChoices())
	if err != nil {
		return m.cronToast(err.Error(), toastError)
	}
	m.cron.form = f
	return m, nil
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
				var apiErr *APIError
				if err != nil && (!errors.As(err, &apiErr) || apiErr == nil || apiErr.Status != http.StatusNotFound) {
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
		write := kataapi.PutCronJobBody{Name: name, Definition: kataapi.CronJobDefinitionFrom(def), Actor: actor}
		notice := "Created " + name
		if !f.create {
			write.ExpectedEventUID, notice = f.expected, "Saved "+name
		}
		if err := api.PutCronJob(ctx, pid, f.uid, write); err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr != nil && apiErr.Status == http.StatusConflict && apiErr.Code == "cron_conflict" {
				if current, readErr := api.ShowCronJob(ctx, pid, f.uid); readErr == nil {
					// A create whose reply was lost conflicts with itself on retry.
					if f.create && current.Name == name && cronSameDefinition(current.Definition, def) {
						if current.Archived {
							return fail(errors.New("this job was created and then archived; use the CLI to restore it"))
						}
						return cronSavedMsg{conn: conn, scope: scope, notice: notice, fromForm: true}
					}
					return cronSavedMsg{conn: conn, scope: scope, conflict: &current, err: err, fromForm: true}
				}
			}
			return fail(err)
		}
		return cronSavedMsg{conn: conn, scope: scope, notice: notice, fromForm: true}
	}
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
	case "enter", "up", "down":
		if f.ed.activeField().kind == fieldMultiLine && !f.isChoice(f.focus) {
			f.ed, _ = f.ed.delegateToField(msg)
			f.validate()
			break
		}
		if key == "up" {
			f.move(-1)
		} else {
			f.move(1)
		}
	case "tab":
		f.move(1)
	case "shift+tab":
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
		f.ed, _ = f.ed.delegateToField(msg)
		f.validate()
		f.general = ""
	}
	return m, nil
}

// A conflict keeps the whole draft until the user reloads or saves it.
func (m Model) routeCronConflictKey(key string) (Model, tea.Cmd) {
	f := m.cron.form
	current := *f.conflict
	switch key {
	case "r":
		if current.Archived {
			return m, nil
		}
		next, err := cronFormFor(current, f.workflows)
		if err != nil {
			f.general = err.Error()
			return m, nil
		}
		m.cron.form = next
	case "ctrl+o", "ctrl+s":
		if current.Archived {
			return m, nil
		}
		f.create, f.expected, f.conflict = false, current.DefinitionEventUID, nil
		f.saving = true
		return m, m.cronSaveCmd(*f)
	case "j", "down":
		f.conflictCursor++
	case "k", "up":
		f.conflictCursor = max(f.conflictCursor-1, 0)
	case "esc":
		f.conflict = nil
	}
	return m, nil
}

func cronFormHelpRows(f *cronForm) []helplayout.HelpItem {
	switch {
	case f.conflict != nil:
		rows := []helplayout.HelpItem{{Key: "↑↓", Description: "scroll"}, {Key: "esc", Description: "back to my draft"}}
		if !f.conflict.Archived {
			rows = slices.Insert(rows, 0, helplayout.HelpItem{Key: "r", Description: "reload current job"}, helplayout.HelpItem{Key: "ctrl+o", Description: "save my draft"})
		}
		return rows
	}
	return []helplayout.HelpItem{
		{Key: "tab/shift+tab", Description: "field"},
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
			"Current name: "+sanitizeForLine(c.Name), "Current event: "+c.DefinitionEventUID)
		if c.Archived {
			lines = append(lines, "Current state: archived")
		}
		raw, err := json.Marshal(c.Definition, jsontext.WithIndent("  "))
		if err == nil {
			for line := range strings.SplitSeq(string(raw), "\n") {
				lines = append(lines, termtext.Wrap(sanitizeForDisplay(line), max(m.width, 1))...)
			}
		}
		f.conflictCursor = min(f.conflictCursor, max(len(lines)-1, 0))
		return lines, f.conflictCursor
	}
	labelWidth := 0
	order := f.order()
	for _, field := range order {
		labelWidth = max(labelWidth, len([]rune(cronFieldLabel(f, field))))
	}
	for _, field := range order {
		if field == f.focus {
			cursor = len(lines)
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
			ed := f.ed.activeField()
			width := max(m.width-labelWidth-5, 1)
			if ed.kind == fieldMultiLine {
				ed.area.SetWidth(width)
				value = ed.area.View()
			} else {
				ed.input.SetWidth(max(width-1, 1))
				ed.input.SetCursor(ed.input.Position())
				value = ed.input.View()
			}
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
		for n, part := range strings.Split(line, "\n") {
			if n > 0 {
				part = strings.Repeat(" ", labelWidth+5) + part
			}
			lines = append(lines, ansi.Truncate(part, m.width, "…"))
		}
	}
	return lines, cursor
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

// cronSameDefinition compares two definitions as the daemon stores them.
func cronSameDefinition(a, b cron.JobDefinition) bool {
	canon := func(d cron.JobDefinition) string {
		raw, err := json.Marshal(d)
		if err != nil {
			return ""
		}
		value := jsontext.Value(raw)
		if value.Canonicalize() != nil {
			return ""
		}
		return string(value)
	}
	ca := canon(a)
	return ca != "" && ca == canon(b)
}
