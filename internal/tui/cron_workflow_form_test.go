package tui

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	katauid "go.kenn.io/kata/internal/uid"
)

func workflowRecord() db.CronWorkflow {
	return db.CronWorkflow{
		CronDefinition: db.CronDefinition{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", Name: "Deploy", DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB3"},
		Definition:     cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "ship", Kind: "prompt", Prompt: "Ship it"}}},
	}
}

func openWorkflow(t *testing.T, rec db.CronWorkflow, detail bool) (Model, *cronWriteTestAPI) {
	t.Helper()
	m, api := cronWriteFixture(t)
	api.workflowShown = rec
	m = sendRune(m, '4')
	if detail {
		m = sendKey(m, tea.KeyEnter)
	}
	m, cmd := press(m, keyRune('e'))
	return settle(t, m, cmd), api
}

func TestWorkflowFormKeysAndAuthority(t *testing.T) {
	m, _ := cronWriteFixture(t)
	m = sendRune(m, '4')
	m, _ = updateModel(m, keyRune('n'))
	require.NotNil(t, m.cron.form)
	require.True(t, m.cron.form.wf)
	require.True(t, katauid.Valid(m.cron.form.uid))
	require.Equal(t, []cronField{cfName, cfDefinition}, m.cron.form.order())
	require.Contains(t, stripANSI(m.viewBody()), "New workflow")
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Nil(t, m.cron.form)
	m = sendRune(m, '2')
	m, _ = updateModel(m, keyRune('n'))
	require.False(t, m.cron.form.wf)
	for _, key := range []rune{'n', 'e'} {
		for _, pending := range []bool{false, true} {
			m, _ := cronWriteFixture(t)
			m = sendRune(m, '4')
			m.issueScoped, m.scopedWritable = !pending, true
			m.authCapabilitiesRequired, m.authCapabilitiesReady = pending, false
			m, _ = updateModel(m, keyRune(key))
			require.Nil(t, m.cron.form)
		}
	}
	for _, detail := range []bool{false, true} {
		m, _ := openWorkflow(t, workflowRecord(), detail)
		require.NotNil(t, m.cron.form)
		require.Equal(t, cfDefinition, m.cron.form.focus)
		require.Contains(t, stripANSI(m.viewBody()), "Edit workflow · Deploy")
	}
}

func TestWorkflowEditorReturnAndValidation(t *testing.T) {
	m, api := openWorkflow(t, workflowRecord(), false)
	api.shown = executeRecord(`{}`)
	api.shown.UID = m.cron.data.jobs[0].UID
	m, cmd := updateModel(m, ctrlKey('e'))
	require.NotNil(t, cmd)
	const bad = `{
  "version":1,
  "steps":[{"key":"ship","kind":"prompt","prompt":"Ship","retries":"bad"}]
}`
	m, _ = updateModel(m, editorReturnedMsg{formGen: m.nextFormGen, content: bad})
	require.Equal(t, bad, m.cron.form.val[cfDefinition])
	view := stripANSI(m.viewBody())
	require.Contains(t, view, "line 3, column 68")
	require.Contains(t, view, "/definition/steps/0/retries")
	m, cmd = press(m, ctrlKey('o'))
	require.Nil(t, cmd)
	require.Equal(t, cfDefinition, m.cron.form.focus)
	const invalid = `{"version":1,"steps":[{"key":"build","kind":"prompt","prompt":"Build"},{"key":"ship","kind":"prompt"}]}`
	m, _ = updateModel(m, editorReturnedMsg{formGen: m.nextFormGen, content: invalid})
	require.Contains(t, stripANSI(m.viewBody()), `step 2 "ship": prompt step requires only prompt`)
	gen := m.nextFormGen
	m, _ = updateModel(m, editorReturnedMsg{formGen: gen - 1, content: "stale"})
	require.Equal(t, invalid, m.cron.form.val[cfDefinition])
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, modalDiscardCronJob, m.modal)
	require.Contains(t, stripANSI(m.View().Content), "Discard draft?")
	m, _ = updateModel(m, keyRune('y'))
	require.Nil(t, m.cron.form)
	m, _ = updateModel(m, editorReturnedMsg{formGen: gen, content: "closed"})
	require.Nil(t, m.cron.form)
	m = sendRune(m, '2')
	m, cmd = press(m, keyRune('e'))
	m = settle(t, m, cmd)
	require.NotNil(t, m.cron.form)
	require.False(t, m.cron.form.wf)
}

func TestWorkflowConflictKeepsDraftAndGuardsReplacement(t *testing.T) {
	m, api := openWorkflow(t, workflowRecord(), false)
	opened, draft := m.cron.form.expected, m.cron.form.val[cfDefinition]
	api.workflowShown.Name = "Current name"
	api.workflowShown.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB8"
	api.workflowShown.Definition.Steps[0].Prompt = "Current prompt"
	api.showErr = errors.New("use the conflict response")
	api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "cron_conflict"}, nil}
	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.NotNil(t, m.cron.form.conflict)
	require.Equal(t, draft, m.cron.form.val[cfDefinition])
	require.Equal(t, opened, api.workflowPuts[0].write.ExpectedEventUID)
	var viewed strings.Builder
	for range 30 {
		viewed.WriteString(stripANSI(m.viewBody()))
		m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	require.Contains(t, viewed.String(), "Current name")
	require.Contains(t, viewed.String(), "Current prompt")
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Nil(t, m.cron.form.conflict)
	require.Equal(t, draft, m.cron.form.val[cfDefinition])
	api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "cron_conflict"}, nil}
	m, cmd = press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	m, cmd = press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FB8", api.workflowPuts[2].write.ExpectedEventUID)
	require.Equal(t, "Ship it", api.workflowPuts[2].write.Definition.Steps[0].Prompt)
}

func TestWorkflowRefusesArchivedAndFutureRecords(t *testing.T) {
	for _, archived := range []bool{false, true} {
		rec := workflowRecord()
		if archived {
			rec.DeletedAt = new(time.Now())
		} else {
			rec.Definition.Version = 2
		}
		m, _ := openWorkflow(t, rec, false)
		require.Nil(t, m.cron.form)
		if archived {
			require.Contains(t, m.toast.text, "restore")
		} else {
			require.Contains(t, m.toast.text, "version")
		}
		m, api := openWorkflow(t, workflowRecord(), false)
		draft, expected := m.cron.form.val[cfDefinition], m.cron.form.expected
		api.workflowShown = rec
		api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "cron_conflict"}}
		m, cmd := press(m, ctrlKey('o'))
		m = settle(t, m, cmd)
		require.NotNil(t, m.cron.form)
		require.Equal(t, draft, m.cron.form.val[cfDefinition])
		require.Equal(t, expected, m.cron.form.expected)
		if archived {
			require.NotNil(t, m.cron.form.conflict)
			require.NotContains(t, stripANSI(m.viewBody()), "save my draft")
			m, cmd = press(m, ctrlKey('o'))
			require.Nil(t, cmd)
		} else {
			require.Nil(t, m.cron.form.conflict)
			require.Contains(t, m.cron.form.general, "version")
		}
		require.Len(t, api.workflowPuts, 1)
	}
}

func TestWorkflowCreateResendKeepsUID(t *testing.T) {
	m, api := cronWriteFixture(t)
	m = sendRune(m, '4')
	m, _ = press(m, keyRune('n'))
	m.cron.form.val[cfName] = "Deploy"
	minted := m.cron.form.uid
	api.putErrs = []error{errors.New("connection reset"), nil}
	m, cmd := press(m, ctrlKey('o'))
	require.True(t, m.cron.form.saving)
	m, ignored := press(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Nil(t, ignored)
	require.NotNil(t, m.cron.form)
	m = settle(t, m, cmd)
	require.Contains(t, m.cron.form.general, "connection reset")
	m, cmd = press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
	require.Len(t, api.workflowPuts, 2)
	for _, put := range api.workflowPuts {
		require.Equal(t, minted, put.uid)
		require.Empty(t, put.write.ExpectedEventUID)
		require.Equal(t, "alice", put.write.Actor)
	}
}

func TestWorkflowEditResponseNeedsItsContext(t *testing.T) {
	for _, away := range []func(Model) Model{
		func(m Model) Model { return sendRune(m, '2') },
		func(m Model) Model { return sendRune(m, '?') },
		func(m Model) Model { m.connGen++; return m },
		func(m Model) Model { m.scopeGen++; return m },
		func(m Model) Model { m.cron.editSeq++; return m },
	} {
		m, api := cronWriteFixture(t)
		api.workflowShown = workflowRecord()
		m = sendRune(m, '4')
		m, cmd := press(m, keyRune('e'))
		m = settle(t, away(m), cmd)
		require.Nil(t, m.cron.form)
	}
}

func TestWorkflowFormRoundTripsThroughDaemon(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	project, err := c.EnsureProject(t.Context(), "workflow-example", "alice")
	require.NoError(t, err)
	m, _ := cronWriteFixture(t)
	m.api, m.scope.projectID = c, project.ID
	m = sendRune(m, '4')
	m, _ = press(m, keyRune('n'))
	m.cron.form.val[cfName] = "Deploy"
	const definition = `{"version":1,"options":{"big":12345678901234567890123,"exact":1.10},"steps":[{"key":"ship","kind":"prompt","prompt":"Ship","options":{"big":12345678901234567890123,"exact":1.10}}]}`
	m.cron.form.val[cfDefinition] = definition
	uid := m.cron.form.uid
	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
	rec, err := c.ShowCronWorkflow(t.Context(), project.ID, uid)
	require.NoError(t, err)
	m.cron.form, err = workflowFormFor(rec)
	require.NoError(t, err)
	m.cron.form.val[cfName] = "Renamed"
	m, cmd = press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
	rec, err = c.ShowCronWorkflow(t.Context(), project.ID, uid)
	require.NoError(t, err)
	require.Equal(t, "Renamed", rec.Name)
	require.Equal(t, "Ship", rec.Definition.Steps[0].Prompt)
	for _, options := range [][]byte{rec.Definition.Options, rec.Definition.Steps[0].Options} {
		require.Contains(t, string(options), "12345678901234567890123")
		require.Contains(t, string(options), "1.10")
	}
}
