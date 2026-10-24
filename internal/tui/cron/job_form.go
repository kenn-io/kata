package cron

// Job forms own job drafts; they never write worker state.

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	native "go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/jsonutil"
	"go.kenn.io/kata/internal/tui/cron/jobform"
	"go.kenn.io/kata/internal/tui/cron/records"
	"go.kenn.io/kata/internal/uid"
)

type writer interface {
	SaveJob(context.Context, records.Job, string) (records.Job, error)
	ShowJob(context.Context, string) (records.Job, error)
	ShowIssue(context.Context, string) (records.IssueView, error)
}
type savePhase uint8

const (
	phaseEditing savePhase = iota
	phaseSaving
	phaseUncertain
	phaseConflict
)

type jobForm struct {
	phase                          savePhase
	creating, bannerFocus          bool
	scroll, discardScroll          int
	errorFocus                     string
	active, buffer, activeOriginal string
	preferredCol                   int
	discard                        bool
	fieldErrors                    map[string]string
	editorContext                  editorContext
	ctx                            context.Context
	writer                         writer
	job                            records.Job
	workflows                      []records.Workflow
	values                         map[string]string
	dirty                          map[string]bool
	raw                            map[string]any
	index, caret, width, height    int
	status                         string
	projectUID                     string
	attempt                        *saveAttempt
	newer                          *records.Job
}

// saveAttempt retains one payload and its reference-resolution state for retries.
type saveAttempt struct {
	job      records.Job
	parent   string
	resolved bool
}

type savedMsg struct {
	job       records.Job
	newer     *records.Job
	err       error
	uncertain bool
	parent    string
}

func newJobForm(ctx context.Context, w writer, j *records.Job, flows []records.Workflow, ec editorContext) *jobForm {
	m := &jobForm{creating: j == nil, ctx: ctx, writer: w, workflows: flows, width: 80, height: 24}
	m.projectUID = ec.ProjectUID
	m.editorContext = ec
	if j != nil {
		m.job = *j
	} else {
		m.job = records.Job{UID: newJobUID(), Definition: jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual","timezone":"UTC"},"action":{"kind":"execute","prompt":""},"issue":{"kind":"per-run","title":"","body":""},"overlap":"forbid","catchup":"latest"}`)}
	}
	d := jsontext.NewDecoder(bytes.NewReader(m.job.Definition))
	if err := json.UnmarshalDecode(d, &m.raw, jsonutil.PreserveNumberLiterals()); err != nil || m.raw == nil {
		m.status = "definition: cannot edit malformed JSON"
		m.raw = map[string]any{}
	}
	draft := jobform.NewDraft(m.raw, m.job.Name)
	m.values, m.dirty = draft.Values, draft.Dirty
	if j == nil {
		if body, err := m.definition(); err == nil {
			m.job.Definition = body
		}
	}
	return m
}

func text(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func get(raw map[string]any, path string) any {
	p := strings.Split(path, ".")
	var v any = raw
	for _, k := range p {
		o, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = o[k]
	}
	return v
}
func put(raw map[string]any, path string, v any) {
	p := strings.Split(path, ".")
	for _, k := range p[:len(p)-1] {
		o, ok := raw[k].(map[string]any)
		if !ok {
			if v == nil {
				return
			}
			o = map[string]any{}
			raw[k] = o
		}
		raw = o
	}
	k := p[len(p)-1]
	if v == nil {
		delete(raw, k)
	} else {
		raw[k] = v
	}
}
func (m *jobForm) Value(k string) string { return m.values[k] }
func (m *jobForm) Set(k, v string) {
	if v != m.values[k] {
		m.values[k] = v
		if v == jobform.NewDraft(m.raw, m.job.Name).Values[k] {
			delete(m.dirty, k)
		} else {
			m.dirty[k] = true
		}
	}
}
func (m *jobForm) Init() tea.Cmd { return nil }
func (m *jobForm) draft() jobform.Draft {
	return jobform.Draft{Raw: m.raw, Values: m.values, Dirty: m.dirty}
}
func (m *jobForm) formContext() jobform.Context {
	return jobform.Context{Workflows: m.workflows, Project: m.editorContext.Project, IssueReference: m.editorContext.IssueReference, Persisted: !m.creating}
}
func (m *jobForm) fields() []jobform.Field { return jobform.Fields(m.draft(), m.formContext()) }
func (m *jobForm) definition() ([]byte, error) {
	raw := map[string]any{}
	decoder := jsontext.NewDecoder(bytes.NewReader(m.job.Definition))
	if err := json.UnmarshalDecode(decoder, &raw, jsonutil.PreserveNumberLiterals()); err != nil {
		return nil, fmt.Errorf("definition: %w", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("definition: must be an object")
	}
	mode := m.Value("mode")
	originalMode := jobform.ModeOf(m.raw)
	modeChanged := mode != originalMode
	issueChanged := m.Value("issue_kind") != text(get(m.raw, "issue.kind")) && (mode == "prompt" || mode == "workflow")
	triggerChanged := m.Value("trigger") != text(get(m.raw, "trigger.kind"))
	if modeChanged {
		for _, path := range []string{"action.prompt", "action.workflow_uid", "action.recipient", "action.message", "issue.uid", "issue.parent_uid", "issue.title", "issue.body", "issue.deadline_offset_seconds", "checkout_key", "timeout_seconds"} {
			put(raw, path, nil)
		}
		kind := "execute"
		if mode == "notify" || mode == "recurring" {
			kind = "notify"
		}
		put(raw, "action.kind", kind)
		issueKind := m.Value("issue_kind")
		if mode == "notify" {
			issueKind = "existing"
		}
		if mode == "recurring" {
			issueKind = "per-run"
		}
		put(raw, "issue.kind", issueKind)
	}
	if issueChanged {
		for _, path := range []string{"issue.uid", "issue.title", "issue.body", "issue.deadline_offset_seconds", "issue.parent_uid"} {
			put(raw, path, nil)
		}
	}
	if triggerChanged {
		for _, path := range []string{"trigger.cron", "trigger.interval_seconds", "trigger.at", "trigger.issue_uid", "trigger.lead_seconds"} {
			put(raw, path, nil)
		}
	}
	for _, f := range m.fields() {
		if f.Key == "name" || f.Key == "mode" || f.Key == "favorite" || f.Kind == jobform.ReadOnly {
			continue
		}
		path := jobform.Path(f.Key, m.Value("trigger"))
		if !m.dirty[f.Key] && !modeChanged && (!issueChanged || !strings.HasPrefix(path, "issue.")) && (!strings.HasPrefix(path, "trigger.") || !triggerChanged) {
			continue
		}
		path, v, err := jobform.Encode(f.Key, m.draft(), m.formContext())
		if err != nil {
			return nil, err
		}
		if path != "" {
			put(raw, path, v)
		}
	}
	if mode == "recurring" && (modeChanged || m.job.DefinitionEventUID == "") {
		for _, path := range []string{"secret_refs", "checkout_key", "timeout_seconds", "issue.deadline_offset_seconds"} {
			put(raw, path, nil)
		}
		put(raw, "overlap", "allow")
		if text(get(raw, "action.recipient")) == "" {
			put(raw, "action.recipient", "current-owner-or-author")
		}
	}
	if modeChanged || issueChanged {
		if mode == "notify" || mode == "recurring" || text(get(raw, "issue.kind")) != "per-run" {
			put(raw, "issue.scheduled_offset_seconds", nil)
		}
	}
	if mode == "notify" && m.Value("issue") != "" {
		put(raw, "issue.kind", "existing")
	}
	if mode == "notify" && (m.Value("trigger") == "issue-scheduled" || m.Value("trigger") == "issue-deadline") && m.Value("issue") == "" {
		delete(raw, "issue")
	}
	return json.Marshal(raw, json.Deterministic(true), jsontext.EscapeForHTML(true), jsontext.AllowInvalidUTF8(true))
}
func (m *jobForm) validate(body []byte) error {
	if strings.TrimSpace(m.Value("name")) == "" {
		return fmt.Errorf("name: required")
	}
	if (m.creating || m.Value("name") != m.job.Name) && m.editorContext.NameTaken != nil && m.editorContext.NameTaken(m.Value("name"), m.job.UID) {
		return fmt.Errorf("name: already exists in this project")
	}
	var raw map[string]any
	decoder := jsontext.NewDecoder(bytes.NewReader(body))
	if err := json.UnmarshalDecode(decoder, &raw, jsonutil.PreserveNumberLiterals()); err != nil {
		return err
	}
	if raw == nil {
		return fmt.Errorf("definition: must be an object")
	}
	// Human issue references are resolved once, before sending. Validate the
	// remaining portable structure without requiring host-side catalogs.
	for _, path := range []string{"issue.uid", "issue.parent_uid", "trigger.issue_uid"} {
		if ref := text(get(raw, path)); ref != "" && !canonicalUID(ref) {
			put(raw, path, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
		}
	}
	data, err := json.Marshal(raw, json.Deterministic(true), jsontext.EscapeForHTML(true), jsontext.AllowInvalidUTF8(true))
	if err != nil {
		return err
	}
	_, err = native.ParseJob(data)
	return err
}
func (m *jobForm) draftChanges() (job records.Job, defChanged, favChanged bool, err error) {
	body, err := m.definition()
	if err != nil {
		return job, false, false, err
	}
	job = m.job
	job.Name = m.Value("name")
	job.Definition = body
	defChanged = m.creating || !records.SameDefinition(m.job, job)
	if !defChanged {
		job.Definition = m.job.Definition
	}
	favChanged = false
	return job, defChanged, favChanged, nil
}
func (m *jobForm) locked() bool   { return m.phase != phaseEditing }
func (m *jobForm) canRetry() bool { return m.phase == phaseUncertain }

func (m *jobForm) save() tea.Cmd {
	if m.locked() && !m.canRetry() {
		return nil
	}
	if m.attempt == nil {
		j, defChanged, _, err := m.draftChanges()
		if err == nil && !defChanged {
			closed := editorClosedMsg{Message: "no changes", Target: editorTarget{ProjectUID: m.projectUID, UID: m.job.UID}}
			return func() tea.Msg { return closed }
		}
		if err == nil {
			var raw map[string]any
			decoder := jsontext.NewDecoder(bytes.NewReader(j.Definition))
			err = json.UnmarshalDecode(decoder, &raw, jsonutil.PreserveNumberLiterals())
			if err == nil && raw == nil {
				err = fmt.Errorf("definition: must be an object")
			}
			if err == nil && raw != nil && get(raw, "action.kind") == "execute" && get(raw, "issue.kind") == "per-run" && strings.TrimSpace(text(get(raw, "issue.title"))) == "" {
				put(raw, "issue.title", j.Name)
				j.Definition, err = json.Marshal(raw, json.Deterministic(true), jsontext.EscapeForHTML(true), jsontext.AllowInvalidUTF8(true))
			}
			if err == nil {
				err = m.validate(j.Definition)
			}
		}
		if err != nil {
			m.status = err.Error()
			m.bannerFocus = true
			m.scroll = 0
			return nil
		}
		m.attempt = &saveAttempt{job: j}
		if m.Value("mode") == "recurring" {
			m.attempt.parent = m.Value("parent")
		}
	}

	retry := m.canRetry()
	a := *m.attempt
	recurring := m.Value("mode") == "recurring"
	ctx, writer := m.ctx, m.writer
	m.phase = phaseSaving
	return func() tea.Msg {
		j, parent := a.job, a.parent
		if retry {
			current, err := writer.ShowJob(ctx, j.UID)
			if err != nil && (j.DefinitionEventUID != "" || records.Status(err) != 404) {
				return savedMsg{job: j, parent: parent, err: err, uncertain: true}
			}
			if err == nil && records.SameDefinition(j, current) {
				return savedMsg{job: current}
			}
			if err == nil && current.DefinitionEventUID != j.DefinitionEventUID {
				return savedMsg{job: j, parent: parent, newer: &current, err: &ConflictError{Pending: j, Newer: current, Cause: fmt.Errorf("definition changed during uncertain save")}}
			}
		}
		// Resolve human refs once. The frozen retry retains canonical UID bytes.
		var raw map[string]any
		decoder := jsontext.NewDecoder(bytes.NewReader(j.Definition))
		if err := json.UnmarshalDecode(decoder, &raw, jsonutil.PreserveNumberLiterals()); err != nil {
			return savedMsg{err: fmt.Errorf("definition: %w", err)}
		}
		if raw == nil {
			return savedMsg{err: fmt.Errorf("definition: must be an object")}
		}
		path := ""
		if recurring {
			path = "issue.parent_uid"
		} else if text(get(raw, "issue.kind")) == "existing" {
			path = "issue.uid"
		}
		if ref := text(get(raw, path)); path != "" && !canonicalUID(ref) {
			issue, err := writer.ShowIssue(ctx, ref)
			if err != nil {
				return savedMsg{err: fmt.Errorf("issue: %w", err)}
			}
			if !canonicalUID(issue.UID) {
				return savedMsg{err: fmt.Errorf("issue: lookup did not return a canonical UID")}
			}
			put(raw, path, issue.UID)
			j.Definition, _ = json.Marshal(raw, json.Deterministic(true), jsontext.EscapeForHTML(true), jsontext.AllowInvalidUTF8(true))
		}

		if ref := text(get(raw, "trigger.issue_uid")); ref != "" && !canonicalUID(ref) {
			issue, err := writer.ShowIssue(ctx, ref)
			if err != nil {
				return savedMsg{err: fmt.Errorf("trigger issue: %w", err)}
			}
			if !canonicalUID(issue.UID) {
				return savedMsg{err: fmt.Errorf("trigger issue: invalid UID")}
			}
			put(raw, "trigger.issue_uid", issue.UID)
			j.Definition, _ = json.Marshal(raw, json.Deterministic(true), jsontext.EscapeForHTML(true), jsontext.AllowInvalidUTF8(true))
		}
		if path == "issue.parent_uid" {
			parent = text(get(raw, path))
		}
		saved, err := writer.SaveJob(ctx, j, parent)
		if err == nil {
			return savedMsg{job: saved}
		}
		if conflict, ok := errors.AsType[*ConflictError](err); ok {
			if records.SameDefinition(j, conflict.Newer) {
				return savedMsg{job: conflict.Newer}
			}
			var newer *records.Job
			if conflict.Newer.UID != "" {
				newer = &conflict.Newer
			}
			return savedMsg{job: j, parent: parent, newer: newer, err: err, uncertain: uncertain(err)}
		}
		if !uncertain(err) && records.Class(err) != records.Conflict {
			return savedMsg{job: j, parent: parent, err: err}
		}
		newer, readErr := writer.ShowJob(ctx, j.UID)
		if status := records.Status(readErr); status == 401 || status == 403 {
			return savedMsg{job: j, parent: parent, err: readErr}
		}
		if readErr == nil && records.SameDefinition(j, newer) {
			return savedMsg{job: newer}
		}
		if records.Class(err) == records.Conflict {
			if readErr == nil && newer.UID != "" {
				return savedMsg{newer: &newer, err: err}
			}
			return savedMsg{err: fmt.Errorf("%w; newer definition unavailable", err)}
		}
		return savedMsg{job: j, parent: parent, err: err, uncertain: uncertain(err)}
	}
}
func (m *jobForm) Update(msg tea.Msg) (form, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
	case savedMsg:
		m.phase = phaseEditing
		if v.err == nil {
			target := editorTarget{ProjectUID: m.projectUID, UID: m.job.UID}
			m.job = v.job
			closed := editorClosedMsg{Message: m.Value("name") + " saved", Target: target, Saved: &v.job}
			return m, func() tea.Msg { return closed }
		}
		m.status = v.err.Error()
		m.bannerFocus = true
		m.scroll = 0
		m.newer = v.newer
		switch {
		case records.Class(v.err) == records.Conflict || v.newer != nil:
			m.phase = phaseConflict
		case v.uncertain:
			m.phase = phaseUncertain
		}
		if v.uncertain {
			m.attempt = &saveAttempt{job: v.job, parent: v.parent, resolved: true}
		} else if m.phase == phaseEditing {
			m.attempt = nil
		}
	case tea.KeyPressMsg:
		return m, m.input(v)
	case tea.PasteMsg:
		m.paste(v.Content)
	}
	return m, nil
}

func (m *jobForm) dirtyDraft() bool {
	if m.active != "" && m.buffer != m.activeOriginal {
		return true
	}
	body, err := m.definition()
	if err != nil {
		return true
	}
	candidate := m.job
	candidate.Name = m.Value("name")
	candidate.Definition = body
	return !records.SameDefinition(m.job, candidate)
}
func closeDraft() tea.Cmd {
	return func() tea.Msg { return editorClosedMsg{Message: "edit cancelled"} }
}
func (m *jobForm) abandon() tea.Cmd {
	if m.dirtyDraft() {
		m.discardScroll = m.scroll
		m.discard = true
		return nil
	}
	return closeDraft()
}
func (m *jobForm) selectKey(key string) {
	fields := m.fields()
	for i, f := range fields {
		if f.Key == key {
			m.index = i
			return
		}
	}
	m.index = max(0, min(m.index, len(fields)-1))
}
func (m *jobForm) setField(f jobform.Field, text string) bool {
	d := m.draft()
	if err := jobform.Set(f, &d, m.formContext(), text); err != nil {
		if m.fieldErrors == nil {
			m.fieldErrors = map[string]string{}
		}
		m.fieldErrors[f.Key] = err.Error()
		m.errorFocus = f.Key
		return false
	}
	m.values, m.dirty = d.Values, d.Dirty
	delete(m.fieldErrors, f.Key)
	m.errorFocus = ""
	m.selectKey(f.Key)
	return true
}
func (m *jobForm) input(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if keys.Down.matchesString(key) || keys.Up.matchesString(key) || keys.ScrollUp.matchesString(key) || keys.ScrollDown.matchesString(key) || keys.NextTab.matchesString(key) || keys.PrevTab.matchesString(key) || keys.Left.matchesString(key) || keys.Right.matchesString(key) {
		m.bannerFocus = false
	}
	m.errorFocus = ""
	if keys.Interrupt.matchesString(key) {
		return tea.Quit
	}
	if m.phase == phaseSaving {
		return nil
	}
	if m.discard {
		switch {
		case matchesKey(key, keys.Yes):
			return closeDraft()
		case matchesKey(key, keys.New, keys.Cancel):
			m.discard = false
			m.scroll = m.discardScroll
		}
		return nil
	}
	if keys.Cancel.matchesString(key) {
		return m.abandon()
	}
	if m.locked() {
		if keys.Quit.matchesString(key) {
			return m.abandon()
		}
		if keys.Save.matchesString(key) && m.canRetry() {
			return m.save()
		}
		if keys.Down.matchesString(key) || keys.ScrollDown.matchesString(key) || keys.NextTab.matchesString(key) {
			m.index = min(len(m.fields())-1, m.index+1)
		}
		if keys.Up.matchesString(key) || keys.ScrollUp.matchesString(key) || keys.PrevTab.matchesString(key) {
			m.index = max(0, m.index-1)
		}
		return nil
	}
	fields := m.fields()
	m.index = max(0, min(m.index, len(fields)-1))
	f := fields[m.index]
	if m.active != "" {
		for _, candidate := range fields {
			if candidate.Key == m.active {
				f = candidate
				break
			}
		}
		if keys.Save.matchesString(key) || keys.Accept.matchesString(key) && f.Kind != jobform.TextArea {
			unchangedRef := (f.Key == "issue" || f.Key == "parent" || f.Key == "run_parent") && m.buffer == m.activeOriginal
			if unchangedRef || m.setField(f, m.buffer) {
				m.active = ""
				m.buffer = ""
			}
			return nil
		}
		r := []rune(m.buffer)
		m.caret = max(0, min(m.caret, len(r)))
		insert := ""
		switch {
		case matchesKey(key, keys.Left, keys.Right, keys.ScrollUp, keys.ScrollDown, keys.Home, keys.End, keys.LineHome, keys.LineEnd, keys.DocHome, keys.DocEnd):
			m.moveCaret(key)
		case matchesKey(key, keys.Backspace):
			if m.caret > 0 {
				m.buffer = string(r[:m.caret-1]) + string(r[m.caret:])
				m.caret--
				m.preferredCol = -1
			}
		case matchesKey(key, keys.DeleteChar):
			if m.caret < len(r) {
				m.buffer = string(r[:m.caret]) + string(r[m.caret+1:])
				m.preferredCol = -1
			}
		case matchesKey(key, keys.Accept):
			if f.Kind == jobform.TextArea {
				insert = "\n"
			}
		case matchesKey(key, keys.Toggle):
			insert = " "
		default:
			insert = typedText(k)
		}
		m.insert(insert)
		return nil
	}
	switch {
	case matchesKey(key, keys.Down, keys.ScrollDown, keys.NextTab):
		m.index = (m.index + 1) % len(fields)
	case matchesKey(key, keys.Up, keys.ScrollUp, keys.PrevTab):
		m.index = (m.index + len(fields) - 1) % len(fields)
	case matchesKey(key, keys.Quit):
		return m.abandon()
	case matchesKey(key, keys.Save, keys.SaveAlt):
		return m.save()
	case matchesKey(key, keys.Back, keys.Left):
		if f.Kind == jobform.Choice {
			m.cycleField(f, -1)
		} else {
			return m.abandon()
		}
	case matchesKey(key, keys.Toggle):
		if f.Kind == jobform.Bool {
			m.setField(f, strconv.FormatBool(m.Value(f.Key) != "true"))
		}
	case matchesKey(key, keys.Open, keys.Right, keys.Accept):
		switch f.Kind {
		case jobform.Bool:
			m.setField(f, strconv.FormatBool(m.Value(f.Key) != "true"))
		case jobform.Choice:
			m.cycleField(f, 1)
		case jobform.ReadOnly:
			m.status = f.Label + " is read-only: " + f.ReadOnlyReason
			m.bannerFocus = true
			m.scroll = 0
		default:
			display, edit := jobform.Get(f, m.draft(), m.formContext())
			m.buffer = edit
			if f.Key == "issue" || f.Key == "parent" || f.Key == "run_parent" {
				m.buffer = display
				if m.buffer == "—" {
					m.buffer = ""
				}
			}
			m.active = f.Key
			m.activeOriginal = m.buffer
			m.caret = len([]rune(m.buffer))
			m.preferredCol = -1
		}
	}
	return nil
}
func (m *jobForm) cycleField(f jobform.Field, direction int) {
	if len(f.Choices) == 0 {
		m.status = f.Label + ": no available choices"
		return
	}
	current := -1
	for i, choice := range f.Choices {
		if choice == m.Value(f.Key) {
			current = i
			break
		}
	}
	next := (current + direction + len(f.Choices)) % len(f.Choices)
	if current < 0 && direction < 0 {
		next = len(f.Choices) - 1
	}
	m.setField(f, f.Choices[next])
}

// typedText is the text a key press types: v1's KeyRunes text, without the
// space bar, which the form handles as "space".
func typedText(k tea.KeyPressMsg) string {
	if k.Code == tea.KeySpace {
		return ""
	}
	return k.Text
}

func (m *jobForm) insert(text string) {
	if text == "" {
		return
	}
	r := []rune(m.buffer)
	m.caret = max(0, min(m.caret, len(r)))
	m.buffer = string(r[:m.caret]) + text + string(r[m.caret:])
	m.caret += len([]rune(text))
	m.preferredCol = -1
}

// paste types bracketed-paste text into the active field, as v1 delivered it
// as typed runes.
func (m *jobForm) paste(text string) {
	m.errorFocus = ""
	if m.locked() || m.discard || m.active == "" {
		return
	}
	m.insert(text)
}

func uncertain(err error) bool {
	return records.Ambiguous(err)
}
func canonicalUID(s string) bool {
	return len(s) == 26 && s[0] <= '7' && strings.Trim(s, "0123456789ABCDEFGHJKMNPQRSTVWXYZ") == ""
}

func newJobUID() string {
	id, err := uid.New()
	if err != nil {
		panic(err)
	}
	return id
}
