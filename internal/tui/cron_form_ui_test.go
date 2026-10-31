package tui

import (
	"context"
	"errors"
	"net/http"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
	katauid "go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronPut struct {
	uid   string
	write CronJobWrite
}

// cronWriteTestAPI doubles the cron write boundary; reads come from the fixture.
type cronWriteTestAPI struct {
	*cronReadTestAPI
	puts      []cronPut
	putErrs   []error // consumed one per put
	shown     CronJobRecord
	archives  []string
	restores  []string
	issueUIDs map[string]string
}

func (a *cronWriteTestAPI) ShowCronJob(context.Context, int64, string) (CronJobRecord, error) {
	return a.shown, nil
}

func (a *cronWriteTestAPI) PutCronJob(_ context.Context, _ int64, uid string, w CronJobWrite) (CronJobRecord, error) {
	a.puts = append(a.puts, cronPut{uid, w})
	if len(a.putErrs) > 0 {
		err := a.putErrs[0]
		a.putErrs = a.putErrs[1:]
		if err != nil {
			return CronJobRecord{}, err
		}
	}
	return CronJobRecord{UID: uid, Name: w.Name, Definition: w.Definition}, nil
}

func (a *cronWriteTestAPI) ArchiveCronJob(_ context.Context, _ int64, uid, _, _ string) error {
	a.archives = append(a.archives, uid)
	return nil
}

func (a *cronWriteTestAPI) RestoreCronJob(_ context.Context, _ int64, uid, _, _ string) error {
	a.restores = append(a.restores, uid)
	return nil
}

func (a *cronWriteTestAPI) ListArchivedCronJobs(context.Context, int64) ([]generated.CronJob, error) {
	return a.data.archived, nil
}

func (a *cronWriteTestAPI) GetIssueDetail(_ context.Context, _ int64, ref string) (*IssueDetail, error) {
	uid, ok := a.issueUIDs[ref]
	if !ok {
		return nil, &APIError{Status: http.StatusNotFound, Code: "not_found"}
	}
	return &IssueDetail{Issue: &Issue{UID: uid}}, nil
}

func cronWriteFixture(t *testing.T) (Model, *cronWriteTestAPI) {
	t.Helper()
	m := cronFixture(t)
	api := &cronWriteTestAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, issueUIDs: map[string]string{"abc4": cronTestIssue}}
	m.api = api
	m.list.actor = "alice"
	return m, api
}

func ctrlKey(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl} }

// press routes a key to the cron screen and returns the command it produced.
func press(m Model, msg tea.KeyPressMsg) (Model, tea.Cmd) { return m.routeCronKey(msg) }

// settle runs the command and delivers the cron result it returns.
func settle(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	require.NotNil(t, cmd)
	switch msg := cmd().(type) {
	case cronSavedMsg:
		m, _ = m.cronSaved(msg)
	case cronEditOpenedMsg:
		m, _ = m.cronEditOpened(msg)
	default:
		t.Fatalf("unexpected message %T", msg)
	}
	return m
}

func TestCronNewJobKeyIsScopedToJobs(t *testing.T) {
	m, _ := cronWriteFixture(t)
	m, _ = updateModel(m, keyRune('n'))
	require.NotNil(t, m.cron.form)
	require.True(t, katauid.Valid(m.cron.form.uid), "the form mints the job UID")
	require.True(t, m.cron.form.create)
	minted := m.cron.form.uid

	m, _ = updateModel(m, keyRune('q'))
	require.Equal(t, modalNone, m.modal, "keys go to the form, not to the global quit")
	require.Contains(t, stripANSI(m.viewBody()), "New job")
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, "q", m.cron.form.val[cfName], "the typed q is in the draft")
	require.Equal(t, minted, m.cron.form.uid)

	runs := sendRune(cronFixture(t), '3')
	runs = sendRune(runs, 'n')
	require.Nil(t, runs.cron.form)
}

func TestCronCreateResendKeepsUID(t *testing.T) {
	m, api := cronWriteFixture(t)
	m, _ = press(m, keyRune('n'))
	f := m.cron.form
	f.val[cfName], f.val[cfParent], f.val[cfTitle] = "Weekly report", "abc4", "Report"
	f.validate()
	api.putErrs = []error{errors.New("connection reset"), nil}

	m, cmd := press(m, ctrlKey('s'))
	m = settle(t, m, cmd)
	require.NotNil(t, m.cron.form, "a failed save keeps the draft")
	require.Contains(t, m.cron.form.general, "connection reset")

	m, cmd = press(m, ctrlKey('s'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
	require.Len(t, api.puts, 2)
	require.Equal(t, api.puts[0].uid, api.puts[1].uid, "resending a create is the same create")
	require.Empty(t, api.puts[1].write.ExpectedEventUID)
	require.Equal(t, cronTestIssue, api.puts[1].write.Definition.Issue.ParentUID, "the parent ref is resolved to its UID")
	require.True(t, api.puts[1].write.Definition.Enabled)
	require.Equal(t, "alice", api.puts[1].write.Actor)
	require.Equal(t, "Created Weekly report", m.toast.text)
}

func TestCronSaveNamesUnknownIssueBesideField(t *testing.T) {
	m, api := cronWriteFixture(t)
	m, _ = press(m, keyRune('n'))
	m.cron.form.val[cfName], m.cron.form.val[cfParent], m.cron.form.val[cfTitle] = "Weekly", "zzzz", "Report"
	m, cmd := press(m, ctrlKey('s'))
	m = settle(t, m, cmd)
	require.Empty(t, api.puts)
	require.Equal(t, cfParent, m.cron.form.err.field)
	require.Contains(t, m.cron.form.err.text, "no issue")
}

func TestCronEditConflictKeepsDraftAndAppliesToCurrent(t *testing.T) {
	m, api := cronWriteFixture(t)
	api.shown = executeRecord(`{"keep":1}`)
	api.shown.UID = m.cron.data.jobs[0].UID
	m, cmd := press(m, keyRune('e'))
	m = settle(t, m, cmd)
	require.NotNil(t, m.cron.form)
	require.False(t, m.cron.form.create)
	opened := api.shown.DefinitionEventUID

	m.cron.form.val[cfTitle] = "My edit"
	api.shown.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB8"
	api.shown.Definition.Enabled = false
	api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "cron_conflict"}, nil}

	m, cmd = press(m, ctrlKey('s'))
	m = settle(t, m, cmd)
	require.Equal(t, opened, api.puts[0].write.ExpectedEventUID, "the edit is guarded on the event it read")
	require.NotNil(t, m.cron.form.conflict)
	require.Equal(t, "My edit", m.cron.form.val[cfTitle], "the draft survives the conflict")
	view := stripANSI(m.viewBody())
	require.Contains(t, view, "01ARZ3NDEKTSV4RRFFQ69G5FB8", "the current job is shown")
	require.Contains(t, view, "apply my draft")

	m, cmd = press(m, keyRune('a'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
	require.Len(t, api.puts, 2)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FB8", api.puts[1].write.ExpectedEventUID, "the draft is applied to the current definition")
	require.Equal(t, "My edit", api.puts[1].write.Definition.Issue.Title)
	require.False(t, api.puts[1].write.Definition.Enabled, "only the user's own changes are applied; the other writer's pause stays")
}

func TestCronEscAsksBeforeDiscardingADirtyDraft(t *testing.T) {
	m, _ := cronWriteFixture(t)
	m, _ = press(m, keyRune('n'))
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Nil(t, m.cron.form, "an untouched form closes at once")

	m, _ = press(m, keyRune('n'))
	m, _ = updateModel(m, keyRune('x'))
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.NotNil(t, m.cron.form)
	require.Equal(t, modalDiscardCronJob, m.modal)
	require.Contains(t, stripANSI(m.View().Content), "Discard job draft?")
	m, _ = updateModel(m, keyRune('n'))
	require.NotNil(t, m.cron.form, "n keeps editing")
	require.Equal(t, modalNone, m.modal)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	m, _ = updateModel(m, keyRune('y'))
	require.Nil(t, m.cron.form)
}

func TestCronFormSavesWithCtrlOAndCtrlS(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{ctrlKey('o'), ctrlKey('s')} {
		m, api := cronWriteFixture(t)
		m, _ = press(m, keyRune('n'))
		m.cron.form.val[cfName], m.cron.form.val[cfParent], m.cron.form.val[cfTitle] = "Weekly", "abc4", "Report"
		m, cmd := press(m, key)
		m = settle(t, m, cmd)
		require.Nil(t, m.cron.form, key.String())
		require.Len(t, api.puts, 1)
	}
	m, _ := cronWriteFixture(t)
	m, _ = press(m, keyRune('n'))
	require.Contains(t, stripANSI(m.viewBody()), "ctrl+o")
}

func TestCronPauseResumeIsGuarded(t *testing.T) {
	m, api := cronWriteFixture(t)
	job := m.cron.data.jobs[0]
	api.shown = executeRecord(`{}`)
	api.shown.DefinitionEventUID = job.DefinitionEventUID

	m, cmd := press(m, keyRune('p'))
	m = settle(t, m, cmd)
	require.Len(t, api.puts, 1)
	require.False(t, api.puts[0].write.Definition.Enabled)
	require.Equal(t, job.DefinitionEventUID, api.puts[0].write.ExpectedEventUID)
	require.Equal(t, "Paused Digest", m.toast.text)

	api.shown.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB8"
	m, cmd = press(m, keyRune('p'))
	m = settle(t, m, cmd)
	require.Len(t, api.puts, 1, "a job that changed since it was loaded is not overwritten")
	require.Contains(t, m.toast.text, "changed since")
}

func TestCronArchiveAndRestoreAskFirst(t *testing.T) {
	m, api := cronWriteFixture(t)
	m, cmd := press(m, keyRune('d'))
	require.Nil(t, cmd)
	require.NotNil(t, m.cron.confirm)
	require.Contains(t, stripANSI(m.viewBody()), "Archive job Daily summary? y/n")
	m, cmd = press(m, keyRune('n'))
	require.Nil(t, cmd)
	require.Empty(t, api.archives)

	m, _ = press(m, keyRune('d'))
	m, cmd = press(m, keyRune('y'))
	m = settle(t, m, cmd)
	require.Equal(t, []string{m.cron.data.jobs[0].UID}, api.archives)
	require.Equal(t, "Archived Daily summary", m.toast.text)

	archived := m.cron.data.jobs[0]
	deleted := archived
	deleted.DeletedAt = &snapshotFixedNow
	m.cron.data.jobs = nil
	m.cron.data.archived = []generated.CronJob{deleted}
	m, _ = press(m, keyRune('a'))
	require.True(t, m.cron.archived)
	m.cron.data.archived = []generated.CronJob{deleted}
	require.Contains(t, stripANSI(m.viewBody()), "archived")

	m, _ = press(m, keyRune('d'))
	require.Nil(t, m.cron.confirm, "an archived job cannot be archived again")
	m, _ = press(m, keyRune('e'))
	require.Nil(t, m.cron.form, "an archived job is restored before it is edited")
	m, _ = press(m, keyRune('u'))
	m, cmd = press(m, keyRune('y'))
	m = settle(t, m, cmd)
	require.Equal(t, []string{archived.UID}, api.restores)
}

func TestCronWriteMessageTellsTokenFromProjectDenial(t *testing.T) {
	revoked := cronWriteMessage(&APIError{Status: http.StatusForbidden, Code: "token_invalid", Message: "token invalid"})
	denied := cronWriteMessage(&APIError{Status: http.StatusForbidden, Code: "scoped_operation_forbidden", Message: "not allowed"})
	require.Contains(t, revoked, "token")
	require.Contains(t, revoked, "revoked")
	require.NotContains(t, revoked, "project")
	require.Contains(t, denied, "project")
	require.NotEqual(t, revoked, denied)
}

func TestCronEditDefinitionRoundTripsThroughDaemon(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	project, err := c.EnsureProject(t.Context(), "cron-ui-example", "alice")
	require.NoError(t, err)
	const options = `{"future":{"keep":[1,2]},"big":12345678901234567890123,"exact":1.10}`
	const jobUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	def := manualExecuteJob(options)
	def.Action.Prompt = "Line one\nLine two"
	_, err = c.PutCronJob(t.Context(), project.ID, jobUID, CronJobWrite{Name: "Digest", Actor: "alice", Definition: def})
	require.NoError(t, err)

	m := newTestModel()
	m.width, m.height = 100, 30
	m.scope = scope{projectID: project.ID}
	m.api = c
	m.list.actor = "alice"
	m = sendRune(m, '2')
	jobs, err := c.ListCronJobs(t.Context(), project.ID)
	require.NoError(t, err)
	m, _ = updateModel(m, cronLoadedMsg(cronData{conn: m.connGen, scope: m.scopeGen, jobs: jobs}))

	m, cmd := press(m, keyRune('e'))
	m = settle(t, m, cmd)
	require.NotNil(t, m.cron.form)
	m.cron.form.val[cfTitle] = "Digest of the day"
	m, cmd = press(m, ctrlKey('s'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)

	after, err := c.ShowCronJob(t.Context(), project.ID, jobUID)
	require.NoError(t, err)
	require.Equal(t, "Digest of the day", after.Definition.Issue.Title)
	require.JSONEq(t, options, string(after.Definition.Options))
	require.Contains(t, string(after.Definition.Options), "12345678901234567890123")
	require.Contains(t, string(after.Definition.Options), "1.10")
	require.Equal(t, "Line one\nLine two", after.Definition.Action.Prompt)
}
