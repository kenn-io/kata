package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func (f *cronForm) noun() string {
	if f.wf {
		return "workflow"
	}
	return "job"
}

func newWorkflowForm() (*cronForm, error) {
	minted, err := uid.New()
	if err != nil {
		return nil, err
	}
	f, err := workflowFormFor(db.CronWorkflow{
		UID:        minted,
		Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "step", Kind: "prompt", Prompt: "Describe the task"}}},
	})
	if err != nil {
		return nil, err
	}
	f.create = true
	f.focusField(cfName)
	return f, nil
}

func workflowFormFor(rec db.CronWorkflow) (*cronForm, error) {
	if err := rec.Definition.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(rec.Definition, jsontext.WithIndent("  "))
	if err != nil {
		return nil, err
	}
	f := &cronForm{wf: true, uid: rec.UID, expected: rec.DefinitionEventUID, openedName: rec.Name}
	f.val[cfName], f.val[cfDefinition] = rec.Name, string(raw)
	f.snapshot, f.snapKind, f.snapTrig = f.val, f.kind, f.trigger
	f.focusField(cfDefinition)
	return f, nil
}

func (f *cronForm) workflowBuild() (string, cron.WorkflowDefinition, error) {
	var def cron.WorkflowDefinition
	text := f.val[cfDefinition]
	if err := json.Unmarshal([]byte(text), &def, json.RejectUnknownMembers(true)); err != nil {
		var offset int64
		var path jsontext.Pointer
		if syntax, ok := errors.AsType[*jsontext.SyntacticError](err); ok {
			offset, path = syntax.ByteOffset, syntax.JSONPointer
		} else if semantic, ok := errors.AsType[*json.SemanticError](err); ok {
			offset, path = semantic.ByteOffset, semantic.JSONPointer
		}
		before := text[:min(max(int(offset), 0), len(text))]
		line := 1 + strings.Count(before, "\n")
		column := utf8.RuneCountInString(before[strings.LastIndex(before, "\n")+1:]) + 1
		return "", def, fieldErr(cfDefinition, "line %d, column %d: /definition%s: %s", line, column, path, err)
	}
	if err := def.Validate(); err != nil {
		return "", def, fieldErr(cfDefinition, "%s", err)
	}
	return strings.TrimSpace(f.val[cfName]), def, nil
}

type workflowOpenedMsg struct {
	conn, scope, seq uint64
	uid              string
	record           db.CronWorkflow
	err              error
}

func (m Model) routeCronWorkflowKey(key string, lines []cronLine, cursor int) (Model, tea.Cmd, bool) {
	if !m.cronScope(cronWorkflows) || m.cron.data == nil {
		return m, nil, false
	}
	if next, cmd, blocked := m.blockMutation(false); blocked {
		return next, cmd, true
	}
	switch key {
	case "n":
		f, err := newWorkflowForm()
		if err != nil {
			m, cmd := m.undoNotice("Could not start a workflow: "+err.Error(), toastError)
			return m, cmd, true
		}
		m.nextFormGen++
		f.formGen = m.nextFormGen
		m.cron.form = f
		return m, nil, true
	case "e":
	default:
		return m, nil, false
	}
	selected, ok := m.cronSelectedUID(lines, cursor)
	if !ok || m.cron.data.workflow(selected) == nil {
		return m, nil, true
	}
	m.cron.editSeq++
	api, pid, conn, scope, seq := m.api, m.scope.projectID, m.connGen, m.scopeGen, m.cron.editSeq
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		rec, err := api.ShowCronWorkflow(ctx, pid, selected)
		return workflowOpenedMsg{conn: conn, scope: scope, seq: seq, uid: selected, record: rec, err: err}
	}, true
}

func (m Model) workflowOpened(msg workflowOpenedMsg) (Model, tea.Cmd) {
	if !m.cronEditAnswerApplies(msg.conn, msg.scope, msg.seq, msg.uid, cronWorkflows) {
		return m, nil
	}
	if msg.err != nil {
		return m.undoNotice("Could not open the workflow: "+msg.err.Error(), toastError)
	}
	if msg.record.DeletedAt != nil {
		return m.undoNotice("The workflow was archived; use the CLI to restore it before editing", toastInfo)
	}
	f, err := workflowFormFor(msg.record)
	if err != nil {
		return m.undoNotice(err.Error(), toastError)
	}
	m.nextFormGen++
	f.formGen = m.nextFormGen
	m.cron.form = f
	return m, nil
}

func (m Model) cronSaveWorkflowCmd(f cronForm) tea.Cmd {
	api, pid, actor, conn, scope := m.api, m.scope.projectID, m.list.actor, m.connGen, m.scopeGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		fail := func(err error) tea.Msg { return cronSavedMsg{conn: conn, scope: scope, err: err} }
		name, def, err := f.workflowBuild()
		if err != nil {
			return fail(err)
		}
		write := kataapi.PutCronWorkflowBody{Name: name, Definition: kataapi.CronWorkflowDefinitionFrom(def), Actor: actor}
		notice := "Created " + name
		if !f.create {
			write.ExpectedEventUID, notice = f.expected, "Saved "+name
		}
		if err := api.PutCronWorkflow(ctx, pid, f.uid, write); err != nil {
			if raw, ok := cronConflictCurrent(err); ok {
				current, readErr := decodeCronWorkflow(raw)
				if readErr == nil {
					readErr = current.Definition.Validate()
				}
				if readErr != nil {
					return fail(readErr)
				}
				return cronSavedMsg{conn: conn, scope: scope, conflict: &cronCurrent{current.CronDefinition, current.Definition}, err: err}
			}
			return fail(err)
		}
		return cronSavedMsg{conn: conn, scope: scope, notice: notice}
	}
}
