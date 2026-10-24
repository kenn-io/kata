package cron

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	defs "go.kenn.io/kata/internal/tui/cron/definition"
	"go.kenn.io/kata/internal/tui/cron/records"
	"go.kenn.io/kata/internal/tui/editorcmd"
)

type workflowWriter interface {
	SaveWorkflow(context.Context, records.Workflow) (records.Workflow, error)
	ShowWorkflow(context.Context, string) (records.Workflow, error)
}
type workflowForm struct {
	wrap func(tea.Msg) tea.Msg

	ctx                     context.Context
	writer                  workflowWriter
	workflow                records.Workflow
	path, status            string
	pathInfo                os.FileInfo
	width, height, scroll   int
	busy, saved             bool
	closed                  bool
	check                   workflowCheck
	checkedBody             jsontext.Value
	newer                   records.Workflow
	newerSummary, newerPath string
	newerInfo               os.FileInfo
	sidecarErr              error
	frozen                  jsontext.Value
}
type workflowMsg struct {
	saved              *records.Workflow
	newer              records.Workflow
	newerSummary       string
	err                error
	uncertain, blocked bool
	body               jsontext.Value
	check              workflowCheck
	sidecarPath        string
	sidecarInfo        os.FileInfo
	sidecarErr         error
}
type editedMsg struct{ err error }

func newWorkflowForm(ctx context.Context, w workflowWriter, workflow records.Workflow) (*workflowForm, error) {
	f, err := os.CreateTemp("", "kata-workflow-*.json")
	if err != nil {
		return nil, err
	}
	m := &workflowForm{ctx: ctx, writer: w, workflow: workflow, path: f.Name(), width: 80, height: 24, status: "edit the draft in $VISUAL or $EDITOR", check: workflowShape(workflow.Definition)}
	m.pathInfo, err = f.Stat()
	if err == nil {
		_, err = f.Write(workflow.Definition)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = m.Close()
		return nil, err
	}
	return m, nil
}

// removeWorkflowFile only removes the regular file this editor created. A
// replaced path, including a symlink, belongs to its new owner.
func removeWorkflowFile(path string, owned os.FileInfo) error {
	if path == "" || owned == nil {
		return nil
	}
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(current, owned) {
		return nil
	}
	return os.Remove(path)
}
func (m *workflowForm) Close() error {
	m.closed = true
	return errors.Join(removeWorkflowFile(m.path, m.pathInfo), removeWorkflowFile(m.newerPath, m.newerInfo))
}

func (m *workflowForm) captureDraft() {
	// A completed editor may have saved by renaming a regular file over the
	// draft. Capture ownership before shutdown can skip editedMsg delivery.
	if info, err := os.Lstat(m.path); err == nil && info.Mode().IsRegular() {
		m.pathInfo = info
	}
}

type workflowEditorCommand struct {
	cmd  *exec.Cmd
	form *workflowForm
}

func (c workflowEditorCommand) Run() error {
	err := c.cmd.Run()
	c.form.captureDraft()
	return err
}
func (c workflowEditorCommand) SetStdin(r io.Reader)  { c.cmd.Stdin = r }
func (c workflowEditorCommand) SetStdout(w io.Writer) { c.cmd.Stdout = w }
func (c workflowEditorCommand) SetStderr(w io.Writer) { c.cmd.Stderr = w }

func (m *workflowForm) editorCommand() *exec.Cmd {
	editor := editorcmd.Resolve()
	return exec.CommandContext(m.ctx, editor[0], append(editor[1:], m.path)...) //nolint:gosec // G204: $EDITOR is the user-configured interactive editor.
}

func (m *workflowForm) edit() tea.Cmd {
	if m.closed {
		return nil
	}
	cmd := m.editorCommand()
	launch := tea.Exec(workflowEditorCommand{cmd: cmd, form: m}, func(err error) tea.Msg {
		if m.wrap != nil {
			return m.wrap(editedMsg{err})
		}
		return editedMsg{err}
	})
	return func() tea.Msg { return ExecMsg{Cmd: launch} }
}
func (m *workflowForm) Init() tea.Cmd { return m.edit() }

func workflowSidecar(path string, previous os.FileInfo, body []byte) (os.FileInfo, error) {
	if previous != nil {
		if err := removeWorkflowFile(path, previous); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // G304: sidecar path belongs to this temporary editor draft.
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		_, err = f.Write(body)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = removeWorkflowFile(path, info)
		return nil, err
	}
	return info, nil
}
func newerWorkflowSummary(w records.Workflow) string {
	summary := w.Name
	if def, err := defs.DecodeWorkflow(w.Definition); err == nil {
		keys := make([]string, 0, min(6, len(def.Steps)))
		for _, s := range def.Steps[:min(6, len(def.Steps))] {
			keys = append(keys, s.Key)
		}
		summary += fmt.Sprintf(" · %d steps: %s", len(def.Steps), strings.Join(keys, ", "))
		if len(def.Steps) > 6 {
			summary += " …"
		}
	}
	return summary
}
func readWorkflowDraft(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is this editor's temporary definition file.
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(f, 256*1024+1))
	closeErr := f.Close()
	return body, errors.Join(readErr, closeErr)
}

func (m *workflowForm) save(force bool) tea.Cmd {
	if m.busy {
		return nil
	}
	m.busy = true
	// Freeze every input before handing the command to Bubble Tea. In particular,
	// a retry cannot read a changed draft or substitute a newer event guard.
	ctx, writer, w, path := m.ctx, m.writer, m.workflow, m.path
	frozen := append(jsontext.Value(nil), m.frozen...)
	priorInfo := m.newerInfo
	return func() tea.Msg {
		body := frozen
		if body == nil {
			var readErr error
			body, readErr = readWorkflowDraft(path)
			if readErr != nil {
				return workflowMsg{err: readErr, blocked: true}
			}
		}
		result := workflowMsg{body: body}
		if len(body) > 256*1024 || !jsontext.Value(body).IsValid() {
			result.err = fmt.Errorf("definition: invalid JSON or exceeds 256 KiB")
			result.blocked = true
			return result
		}
		result.check = workflowShape(body)
		if !force && (result.check.DecodeErr != nil || len(result.check.Unsupported) > 0) {
			result.err = fmt.Errorf("workflow CHECK needs attention; ctrl+s saves with the original guard")
			result.blocked = true
			return result
		}
		w.Definition = body
		if frozen != nil {
			current, err := writer.ShowWorkflow(ctx, w.UID)
			if err != nil {
				result.err = err
				result.uncertain = true
				return result
			}
			if records.SameDefinition(w, current) {
				result.saved = &current
				return result
			}
			if current.DefinitionEventUID != w.DefinitionEventUID {
				result.err = &ConflictError{Pending: w, Newer: current, Cause: fmt.Errorf("definition changed during uncertain save")}
				result.newer = current
				result.newerSummary = newerWorkflowSummary(current)
				return result
			}
		}
		saved, saveErr := writer.SaveWorkflow(ctx, w)
		result.err = saveErr
		if result.err == nil {
			result.saved = &saved
			return result
		}
		var newer records.Workflow
		var readErr error
		if conflict, ok := errors.AsType[*ConflictError](result.err); ok && conflict.Newer.UID != "" {
			newer = conflict.Newer
		} else if uncertain(result.err) || records.Class(result.err) == records.Conflict {
			newer, readErr = writer.ShowWorkflow(ctx, w.UID)
		} else {
			return result
		}
		if status := records.Status(readErr); status == 401 || status == 403 {
			result.err = readErr
			return result
		}
		if readErr == nil && records.SameDefinition(records.Job(w), records.Job(newer)) {
			result.err = nil
			result.saved = &newer
			return result
		}
		result.uncertain = uncertain(result.err) || frozen != nil
		if readErr == nil {
			result.newer = newer
			result.newerSummary = newerWorkflowSummary(newer)
			result.sidecarInfo, result.sidecarErr = workflowSidecar(path+".newer.json", priorInfo, newer.Definition)
			if result.sidecarErr == nil {
				result.sidecarPath = path + ".newer.json"
			}
		}
		return result
	}
}
func (m *workflowForm) finish(message string) tea.Cmd {
	if err := m.Close(); err != nil {
		m.status = "cleanup: " + err.Error()
		return nil
	}
	closed := editorClosedMsg{Message: message, Target: editorTarget{ProjectUID: m.workflow.ProjectUID, UID: m.workflow.UID}}
	if m.saved {
		w := m.workflow
		closed.SavedWorkflow = &w
	}
	return func() tea.Msg { return closed }
}
func (m *workflowForm) Update(msg tea.Msg) (form, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
	case editedMsg:
		m.captureDraft()
		if v.err != nil {
			m.status = v.err.Error()
		} else {
			body, err := readWorkflowDraft(m.path)
			if err != nil {
				m.status = err.Error()
				return m, nil
			}
			if bytes.Equal(body, m.workflow.Definition) {
				return m, m.finish("workflow unchanged; nothing saved")
			}
			return m, m.save(false)
		}
	case workflowMsg:
		m.busy = false
		if v.body != nil {
			m.check, m.checkedBody = v.check, v.body
		}
		if v.err == nil {
			if v.saved != nil {
				v.saved.Project, v.saved.ProjectUID = m.workflow.Project, m.workflow.ProjectUID
				m.workflow = *v.saved
			}
			m.saved = true
			m.status = "workflow saved"
			return m, m.finish(m.status)
		}
		m.status = v.err.Error()
		if !v.blocked {
			m.newer, m.newerSummary = v.newer, v.newerSummary
			m.sidecarErr = v.sidecarErr
			if v.sidecarPath != "" {
				m.newerPath, m.newerInfo = v.sidecarPath, v.sidecarInfo
			}
			if v.uncertain {
				m.frozen = append(jsontext.Value(nil), v.body...)
			}
		}
		if m.frozen != nil {
			m.status += " · uncertain outcome; ctrl+s retries the identical draft"
		}
	case tea.KeyPressMsg:
		if m.busy {
			return m, nil
		}
		switch {
		case matchesKey(v.String(), keys.Cancel, keys.Quit):
			return m, m.finish("workflow edit cancelled")
		case matchesKey(v.String(), keys.Edit):
			if m.frozen == nil {
				return m, m.edit()
			}
		case matchesKey(v.String(), keys.Save):
			return m, m.save(true)
		case matchesKey(v.String(), keys.PageDown, keys.HalfDown):
			m.scroll += max(1, m.height/2)
		case matchesKey(v.String(), keys.PageUp, keys.HalfUp):
			m.scroll = max(0, m.scroll-max(1, m.height/2))
		case matchesKey(v.String(), keys.Down, keys.ScrollDown):
			m.scroll++
		case matchesKey(v.String(), keys.Up, keys.ScrollUp):
			m.scroll = max(0, m.scroll-1)
		}
	}
	return m, nil
}
func workflowEditorText(d *detail, text string, width int, role lipgloss.Style) {
	for _, line := range wrapText(text, max(1, width-2)) {
		d.Styled(bodyLine{Text: "  " + role.Render(line)})
	}
}
func (m *workflowForm) View() string {
	d := newDetail(m.width, 14, detailSelection{}, noSelection, nil)
	d.Title("edit workflow "+m.workflow.Name, "", titleStyle)
	d.Subtitle(m.path)
	d.Section("STATUS")
	role := failStyle
	if m.saved {
		role = passStyle
	} else if m.frozen != nil || m.busy {
		role = attentionStyle
	} else if m.status == "edit the draft in $VISUAL or $EDITOR" {
		role = subtleStyle
	}
	workflowEditorText(d, m.status, m.width, role)
	d.Section("CHECK")
	if m.check.DecodeErr != nil {
		workflowEditorText(d, "! does not parse: "+m.check.DecodeErr.Error(), m.width, failStyle)
	} else if len(m.checkedBody) > 256*1024 || m.checkedBody != nil && !jsontext.Value(m.checkedBody).IsValid() {
		workflowEditorText(d, "! invalid JSON or exceeds 256 KiB", m.width, failStyle)
	} else {
		if len(m.check.Unsupported) == 0 {
			workflowEditorText(d, "local workflow checks passed", m.width, passStyle)
		}
		for _, reason := range m.check.Unsupported {
			workflowEditorText(d, "! unsupported workflow: "+reason, m.width, failStyle)
		}
		for _, warning := range m.check.Warnings {
			workflowEditorText(d, warning, m.width, attentionStyle)
		}
		for _, reason := range m.check.JobDependent {
			workflowEditorText(d, "per job: "+reason, m.width, subtleStyle)
		}
	}
	if m.newerSummary != "" {
		d.Section("NEWER IN KATA")
		workflowEditorText(d, m.newerSummary, m.width, detailMetaStyle)
		if m.sidecarErr != nil {
			workflowEditorText(d, "newer copy unavailable: "+m.sidecarErr.Error(), m.width, failStyle)
		} else if m.newerPath != "" {
			workflowEditorText(d, m.newerPath, m.width, subtleStyle)
		}
	}
	items := []helpItem{hintGroup("e", "edit again", 80, keys.Edit), hintGroup("ctrl+s", "save", 100, keys.Save, keys.SaveAlt), keys.Cancel.hint(1000)}
	help := footerHelp{Full: items, Short: items}
	p := pane{Body: d.Body(), Bottom: bodyLines{{Text: help.Render(m.width)}}, Width: m.width, Scroll: &m.scroll}
	return p.Render(m.height).Text
}
