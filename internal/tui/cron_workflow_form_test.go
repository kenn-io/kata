package tui

import (
	"errors"
	"net/http"
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
		UID:                "01ARZ3NDEKTSV4RRFFQ69G5FAW",
		Name:               "Deploy",
		DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB3",
		Definition:         cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "ship", Kind: "prompt", Prompt: "Ship it"}}},
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
	for _, detail := range []bool{false, true} {
		m, _ := openWorkflow(t, workflowRecord(), detail)
		require.NotNil(t, m.cron.form)
		require.Equal(t, cfDefinition, m.cron.form.focus)
		require.Contains(t, stripANSI(m.viewBody()), "Edit workflow · Deploy")
	}
}

func TestWorkflowEditorReturnAndValidation(t *testing.T) {
	m, _ := openWorkflow(t, workflowRecord(), false)
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
	_, err := cron.ParseWorkflow([]byte(invalid))
	require.Error(t, err)
	require.Contains(t, stripANSI(m.viewBody()), err.Error())
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
	require.Contains(t, stripANSI(m.viewBody()), "Current name")
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
	t.Run("archived", func(t *testing.T) {
		rec := workflowRecord()
		rec.DeletedAt = new(time.Now())
		m, _ := openWorkflow(t, rec, false)
		require.Nil(t, m.cron.form)
		require.Contains(t, m.toast.text, "restore")

		m, api := openWorkflow(t, workflowRecord(), false)
		api.workflowShown = rec
		api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "cron_conflict"}}
		m, cmd := press(m, ctrlKey('o'))
		m = settle(t, m, cmd)
		require.NotNil(t, m.cron.form.conflict)
		require.NotContains(t, stripANSI(m.viewBody()), "save my draft")
		m, cmd = press(m, ctrlKey('o'))
		require.Nil(t, cmd)
		require.Len(t, api.workflowPuts, 1)
	})
	t.Run("future version", func(t *testing.T) {
		rec := workflowRecord()
		rec.Definition.Version = 2
		m, _ := openWorkflow(t, rec, false)
		require.Nil(t, m.cron.form)
		require.Contains(t, m.toast.text, "version")

		m, api := openWorkflow(t, workflowRecord(), false)
		draft, expected := m.cron.form.val[cfDefinition], m.cron.form.expected
		api.workflowShown = rec
		api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "cron_conflict"}}
		m, cmd := press(m, ctrlKey('o'))
		m = settle(t, m, cmd)
		require.NotNil(t, m.cron.form)
		require.Equal(t, draft, m.cron.form.val[cfDefinition])
		require.Equal(t, expected, m.cron.form.expected)
		require.Nil(t, m.cron.form.conflict)
		require.Contains(t, m.cron.form.general, "version")
		require.Len(t, api.workflowPuts, 1)
	})
}

func TestWorkflowCreatePut(t *testing.T) {
	m, api := cronWriteFixture(t)
	m = sendRune(m, '4')
	m, _ = press(m, keyRune('n'))
	m.cron.form.val[cfName] = "Deploy"
	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
	require.Len(t, api.workflowPuts, 1)
	require.Empty(t, api.workflowPuts[0].write.ExpectedEventUID)
	require.Equal(t, "alice", api.workflowPuts[0].write.Actor)
}

func TestWorkflowEditResponseNeedsItsContext(t *testing.T) {
	m, api := cronWriteFixture(t)
	api.workflowShown = workflowRecord()
	m = sendRune(m, '4')
	m, cmd := press(m, keyRune('e'))
	m = settle(t, sendRune(m, '2'), cmd)
	require.Nil(t, m.cron.form)
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
	const definition = `{"version":1,"steps":[{"key":"ship","kind":"prompt","prompt":"Ship"}]}`
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
	require.Equal(t, "Saved Renamed", m.toast.text)
}
