package cron

import (
	"context"
	"slices"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"go.kenn.io/kata/internal/tui/cron/records"
)

type form interface {
	Init() tea.Cmd
	Update(tea.Msg) (form, tea.Cmd)
	View() string
}

// Service is the captured native definition service for one project.
type Service interface {
	writer
	workflowWriter
	Pause(records.Job) (records.Job, error)
	Delete(records.Job) error
}
type editorContext struct {
	IssueReference      func(string) string
	ProjectUID, Project string
	NameTaken           func(string, string) bool
}
type editorTarget struct{ ProjectUID, UID string }
type editorClosedMsg struct {
	Message       string
	Target        editorTarget
	Saved         *records.Job
	SavedWorkflow *records.Workflow
}

// ExecMsg asks the host to run an interactive editor outside its result envelope.
type ExecMsg struct {
	Cmd        tea.Cmd
	generation uint64
	project    string
}

// ExecCurrent checks authority before launching an interactive editor.
func (m *model) ExecCurrent(msg ExecMsg) bool {
	workflow, ok := m.editor.(*workflowForm)
	return ok && !workflow.closed && msg.generation == m.editorGeneration && msg.project == m.editorProject
}

// ChangedMsg invalidates native catalogs after an acknowledged definition change.
type ChangedMsg struct {
	Saved, Deleted *records.Job
	SavedWorkflow  *records.Workflow
	ProjectKey     string
	Generation     uint64
}

// ChangeCurrent rejects queued mutations from discarded project authority.
func (m *model) ChangeCurrent(msg ChangedMsg) bool {
	return msg.Generation == m.projectGenerations[msg.ProjectKey]
}

// AccessLostMsg fences protected content and drafts after authentication loss.
type AccessLostMsg struct {
	Status     int
	Code       string
	Message    string
	ProjectKey string
}

// ConflictError retains the source draft and native current document.
type ConflictError = records.ConflictError

type formState struct {
	editor             form
	editorProject      string
	editorCreating     bool
	editorGeneration   uint64
	confirm            []records.Job
	confirmMode        string
	conflict           *ConflictError
	modalScroll        int
	busy               bool
	busyProject        string
	projectGenerations map[string]uint64
	projectChoices     []string
	projectCursor      int
	projectScroll      int
}

type editorResult struct {
	generation uint64
	project    string
	msg        tea.Msg
}

func wrapEditorCommand(cmd tea.Cmd, generation uint64, project string) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			out := make(tea.BatchMsg, len(batch))
			for i, child := range batch {
				out[i] = wrapEditorCommand(child, generation, project)
			}
			return out
		}
		switch launch := msg.(type) {
		case ExecMsg:
			launch.generation, launch.project = generation, project
			return launch
		case tea.QuitMsg:
			return msg
		}
		return editorResult{generation, project, msg}
	}
}

func (m *model) wrapEditorCmd(cmd tea.Cmd) tea.Cmd {
	return wrapEditorCommand(cmd, m.editorGeneration, m.editorProject)
}

type jobPausedMsg struct {
	generation uint64
	project    string
	saved      records.Job
	err        error
}
type jobDeletedMsg struct {
	generation uint64
	job        records.Job
	err        error
}

func (m *model) SetServices(services map[string]Service, readOnly bool, wrap func(tea.Msg) tea.Msg) {
	m.opts.Services = services
	m.opts.ReadOnly = readOnly
	m.opts.Wrap = wrap
}
func (m *model) Close() error {
	if c, ok := m.editor.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ForgetProject discards protected form state after project access is lost.
func (m *model) ForgetProject(project string) {
	if m.projectGenerations == nil {
		m.projectGenerations = map[string]uint64{}
	}
	m.projectGenerations[project]++
	if m.busyProject == project {
		m.busy = false
		m.busyProject = ""
	}
	if m.editorProject == project {
		m.editorGeneration++
		if err := m.Close(); err != nil {
			m.notify(statusError, err.Error())
		}
		m.editor = nil
		m.editorProject = ""
	}
	m.confirm = slices.DeleteFunc(m.confirm, func(j records.Job) bool { return j.ProjectUID == project })
	if m.conflict != nil && (m.conflict.Pending.ProjectUID == project || m.conflict.Newer.ProjectUID == project) {
		m.conflict = nil
	}
	m.projectChoices = slices.DeleteFunc(m.projectChoices, func(key string) bool { return key == project })
	m.projectCursor = max(0, min(m.projectCursor, len(m.projectChoices)-1))
}

func (m *model) workflowsFor(project string) []records.Workflow {
	var flows []records.Workflow
	for _, w := range m.data.Workflows {
		if w.ProjectUID == project {
			flows = append(flows, w)
		}
	}
	return flows
}
func (m *model) editorContext(key string) editorContext {
	return editorContext{ProjectUID: key, Project: m.projectName(key), IssueReference: func(uid string) string { return m.knownIssueRef(key, uid) }, NameTaken: func(name, uid string) bool {
		for _, j := range m.data.Jobs {
			if j.ProjectUID == key && j.UID != uid && strings.EqualFold(strings.TrimSpace(j.Name), strings.TrimSpace(name)) {
				return true
			}
		}
		return false
	}}
}
func (m *model) openJobEditor(key string, j *records.Job) tea.Cmd {
	svc := m.opts.Services[key]
	if svc == nil || m.opts.ReadOnly {
		m.notify(statusInfo, "read-only: definition editing unavailable")
		return nil
	}
	m.editor = newJobForm(context.Background(), svc, j, m.workflowsFor(key), m.editorContext(key))
	m.editorGeneration++
	m.editorProject = key
	m.editorCreating = j == nil
	m.editor.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	return m.wrapEditorCmd(m.editor.Init())
}
func (m *model) editWorkflow(v *workflowView) tea.Cmd {
	if v == nil {
		return nil
	}
	svc := m.opts.Services[v.key.projectUID]
	if svc == nil || m.opts.ReadOnly {
		m.notify(statusInfo, "read-only: definition editing unavailable")
		return nil
	}
	f, err := newWorkflowForm(context.Background(), svc, m.data.Workflows[v.index])
	if err != nil {
		m.notify(statusError, err.Error())
		return nil
	}
	m.editor = f
	m.editorGeneration++
	m.editorProject = v.key.projectUID
	generation, project, wrap := m.editorGeneration, m.editorProject, m.opts.Wrap
	f.wrap = func(msg tea.Msg) tea.Msg {
		result := editorResult{generation, project, msg}
		if wrap != nil {
			return wrap(result)
		}
		return result
	}
	f.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	return m.wrapEditorCmd(f.Init())
}
func (m *model) newJob() tea.Cmd {
	keys := make([]string, 0, len(m.data.Projects))
	for key := range m.data.Projects {
		if m.opts.Services[key] != nil {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return m.projectName(keys[i]) < m.projectName(keys[j]) })
	if len(keys) == 1 {
		return m.openJobEditor(keys[0], nil)
	}
	if len(keys) == 0 {
		m.notify(statusInfo, "no project accepts new jobs")
		return nil
	}
	m.projectChoices = keys
	m.projectCursor = 0
	m.projectScroll = 0
	return nil
}
func (m *model) definitionKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "n":
		if m.currentView() == viewList && m.tab == 0 {
			return m.newJob(), true
		}
		if m.tab == 2 {
			m.notify(statusInfo, "workflows are created and deleted with kata cron workflow …")
			return nil, true
		}
	case "e":
		if v := m.selectedJobView(); v != nil {
			return m.openJobEditor(v.key.projectUID, &v.job), true
		}
		if workflow := m.selectedWorkflow(); workflow != nil {
			return m.editWorkflow(workflow), true
		}
	case "p", "D":
		if m.selectedJobView() != nil {
			return m.jobKeyAction(k.String()), true
		}
		if m.selectedWorkflow() != nil && k.String() == "D" {
			m.notify(statusInfo, "workflows are created and deleted with kata cron workflow …")
		}
		return nil, true
	}
	return nil, false
}
func (m *model) jobKeyAction(key string) tea.Cmd {
	if m.busy || m.opts.ReadOnly {
		m.notify(statusInfo, "read-only or waiting for the current action")
		return nil
	}
	v := m.selectedJobView()
	if v == nil {
		return nil
	}
	svc := m.opts.Services[v.key.projectUID]
	if svc == nil {
		m.notify(statusInfo, "project unavailable")
		return nil
	}
	j := v.job
	if key == "D" {
		m.confirm = []records.Job{j}
		m.confirmMode = "delete"
		return nil
	}
	if key == "p" {
		m.busy = true
		m.busyProject = j.ProjectUID
		generation := m.projectGenerations[j.ProjectUID]
		return func() tea.Msg {
			saved, err := svc.Pause(j)
			return jobPausedMsg{generation: generation, project: j.ProjectUID, saved: saved, err: err}
		}
	}
	return nil
}
func (m *model) confirmJobKey(key string) tea.Cmd {
	jobs := slices.Clone(m.confirm)
	m.confirm = nil
	if key != "y" && key != "Y" {
		return nil
	}
	if len(jobs) == 0 || m.busy || m.opts.ReadOnly {
		return nil
	}
	j := jobs[0]
	svc := m.opts.Services[j.ProjectUID]
	if svc == nil {
		return nil
	}
	m.busy = true
	m.busyProject = j.ProjectUID
	generation := m.projectGenerations[j.ProjectUID]
	return func() tea.Msg { return jobDeletedMsg{generation: generation, job: j, err: svc.Delete(j)} }
}
func jobNames(jobs []records.Job) string {
	names := []string{}
	for _, j := range jobs {
		names = append(names, clean(j.Name))
	}
	return strings.Join(names, ", ")
}
func (m *model) handleFormMessage(msg tea.Msg) (tea.Cmd, bool) {
	var err error
	project := m.editorProject
	staleEditor := false
	if result, ok := msg.(editorResult); ok {
		project, msg = result.project, result.msg
		staleEditor = result.generation != m.editorGeneration || project != m.editorProject || m.editor == nil
	}
	switch v := msg.(type) {
	case savedMsg:
		err = v.err
	case workflowMsg:
		err = v.err
	case jobPausedMsg:
		project = v.project
		err = v.err
	case jobDeletedMsg:
		project = v.job.ProjectUID
		err = v.err
	}
	switch v := msg.(type) {
	case jobPausedMsg:
		if v.generation != m.projectGenerations[v.project] {
			return nil, true
		}
	case jobDeletedMsg:
		if v.generation != m.projectGenerations[v.job.ProjectUID] {
			return nil, true
		}
	}
	if staleEditor {
		return nil, true
	}
	if status := records.Status(err); status == 401 || status == 403 {
		m.busy = false
		return func() tea.Msg {
			return AccessLostMsg{Status: status, Code: records.ErrorCode(err), Message: err.Error(), ProjectKey: project}
		}, true
	}
	switch v := msg.(type) {
	case editorClosedMsg:
		if c, ok := m.editor.(interface{ Close() error }); ok {
			if err := c.Close(); err != nil {
				m.notify(statusError, err.Error())
			}
		}
		m.editor = nil
		m.editorGeneration++
		m.editorProject = ""
		m.projectChoices = nil
		if v.Saved != nil {
			j := *v.Saved
			j.ProjectUID = v.Target.ProjectUID
			j.Project = m.projectName(j.ProjectUID)
			m.acceptSavedJob(j)
		}
		if v.SavedWorkflow != nil {
			w := *v.SavedWorkflow
			found := false
			for i, old := range m.data.Workflows {
				if old.ProjectUID == w.ProjectUID && old.UID == w.UID {
					m.data.Workflows[i] = w
					found = true
					break
				}
			}
			if !found {
				m.data.Workflows = append(m.data.Workflows, w)
			}
			m.reproject()
			m.rebuild()
		}
		m.notify(statusSuccess, v.Message)
		change := ChangedMsg{Saved: v.Saved, SavedWorkflow: v.SavedWorkflow, ProjectKey: project, Generation: m.projectGenerations[project]}
		return func() tea.Msg { return change }, true
	case jobPausedMsg:
		m.busy = false
		if v.err != nil {
			m.notify(statusError, v.err.Error())
			if conflict, ok := v.err.(*ConflictError); ok {
				m.conflict = conflict
			}
			return nil, true
		}
		m.acceptSavedJob(v.saved)
		return func() tea.Msg { return ChangedMsg{Saved: &v.saved, ProjectKey: v.project, Generation: v.generation} }, true
	case jobDeletedMsg:
		m.busy = false
		if v.err != nil {
			m.notify(statusError, v.err.Error())
			if c, ok := v.err.(*ConflictError); ok {
				m.conflict = c
			}
			return nil, true
		}
		m.data.Jobs = slices.DeleteFunc(m.data.Jobs, func(j records.Job) bool { return j.ProjectUID == v.job.ProjectUID && j.UID == v.job.UID })
		m.reproject()
		m.rebuild()
		if current := m.nav[len(m.nav)-1]; current.view == viewJob && current.projectUID == v.job.ProjectUID && current.uid == v.job.UID {
			m.popDetail()
		}
		return func() tea.Msg {
			return ChangedMsg{Deleted: &v.job, ProjectKey: v.job.ProjectUID, Generation: v.generation}
		}, true
	}
	if m.editor != nil {
		var cmd tea.Cmd
		m.editor, cmd = m.editor.Update(msg)
		return m.wrapEditorCmd(cmd), true
	}
	return nil, false
}
func (m *model) acceptSavedJob(j records.Job) {
	found := false
	for i, old := range m.data.Jobs {
		if old.ProjectUID == j.ProjectUID && old.UID == j.UID {
			j.LastRun, j.LastRunKnown = old.LastRun, old.LastRunKnown
			m.data.Jobs[i] = j
			found = true
			break
		}
	}
	if !found {
		m.data.Jobs = append(m.data.Jobs, j)
	}
	m.reproject()
	m.rebuild()
}
func (m *model) formKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	if m.editor != nil {
		var cmd tea.Cmd
		m.editor, cmd = m.editor.Update(k)
		return m.wrapEditorCmd(cmd), true
	}
	if len(m.projectChoices) > 0 {
		switch k.String() {
		case "esc":
			m.projectChoices = nil
		case "j", "down", "tab":
			m.projectCursor = (m.projectCursor + 1) % len(m.projectChoices)
		case "k", "up", "shift+tab":
			m.projectCursor = (m.projectCursor + len(m.projectChoices) - 1) % len(m.projectChoices)
		case "enter":
			key := m.projectChoices[m.projectCursor]
			m.projectChoices = nil
			return m.openJobEditor(key, nil), true
		}
		return nil, true
	}
	if m.conflict != nil {
		switch k.String() {
		case "esc":
			m.conflict = nil
		case "j", "down":
			m.modalScroll++
		case "k", "up":
			m.modalScroll = max(0, m.modalScroll-1)
		}
		return nil, true
	}
	if len(m.confirm) > 0 {
		return m.confirmJobKey(k.String()), true
	}
	return nil, false
}
