package tui

import (
	"context"
	"errors"
	"net/http"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
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
	showErr   error
	archives  []string
	restores  []string
	issueUIDs map[string]string
}

func (a *cronWriteTestAPI) ShowCronJob(context.Context, int64, string) (CronJobRecord, error) {
	return a.shown, a.showErr
}

func (a *cronWriteTestAPI) PutCronJob(_ context.Context, _ int64, uid string, w CronJobWrite) error {
	a.puts = append(a.puts, cronPut{uid, w})
	if len(a.putErrs) > 0 {
		err := a.putErrs[0]
		a.putErrs = a.putErrs[1:]
		return err
	}
	return nil
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
	require.Nil(t, cmd, "applying only rebases the draft; nothing is written yet")
	require.NotNil(t, m.cron.form)
	require.Nil(t, m.cron.form.conflict)
	require.Len(t, api.puts, 1)

	m, cmd = press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
	require.Len(t, api.puts, 2)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FB8", api.puts[1].write.ExpectedEventUID, "the draft is applied to the current definition")
	require.Equal(t, "My edit", api.puts[1].write.Definition.Issue.Title)
	require.False(t, api.puts[1].write.Definition.Enabled, "only the user's own changes are applied; the other writer's pause stays")
}

// conflictAndApply saves the open edit against a stale guard, then applies
// the draft to theirs and returns the form for review.
func conflictAndApply(t *testing.T, m Model, api *cronWriteTestAPI, theirs CronJobRecord) Model {
	t.Helper()
	theirs.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB8"
	api.shown = theirs
	api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "cron_conflict"}}
	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.NotNil(t, m.cron.form.conflict)
	m, cmd = press(m, keyRune('a'))
	require.Nil(t, cmd)
	require.Len(t, api.puts, 1, "applying writes nothing")
	return m
}

func openEdit(t *testing.T, record CronJobRecord) (Model, *cronWriteTestAPI) {
	t.Helper()
	m, api := cronWriteFixture(t)
	api.shown = record
	api.shown.UID = m.cron.data.jobs[0].UID
	m, cmd := press(m, keyRune('e'))
	return settle(t, m, cmd), api
}

func TestCronConflictApplyNamesPropertiesBothSidesChanged(t *testing.T) {
	m, api := openEdit(t, executeRecord(`{}`))
	m.cron.form.val[cfTitle] = "My title"
	theirs := executeRecord(`{}`)
	theirs.Definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: cronTestIssue}
	m = conflictAndApply(t, m, api, theirs)
	require.Contains(t, m.cron.form.general, "Both changed: issue")
	require.Equal(t, "My title", m.cron.form.val[cfTitle], "the user's whole issue policy wins")
}

func TestCronConflictApplyKeepsAChosenScheduleEqualToTheDefault(t *testing.T) {
	m, api := openEdit(t, executeRecord(`{}`))
	m.cron.form.trigger = "cron" // the form's default schedule is 0 9 * * *
	theirs := executeRecord(`{}`)
	theirs.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 8 * * *", Timezone: "UTC"}
	m = conflictAndApply(t, m, api, theirs)
	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Equal(t, "0 9 * * *", api.puts[1].write.Definition.Trigger.Cron)
}

func TestCronConflictApplyKeepsANotificationTargetWhenTheTriggerBecameADate(t *testing.T) {
	const wasTarget, newTarget, source = "01ARZ3NDEKTSV4RRFFQ69G5FB5", cronTestIssue, "01ARZ3NDEKTSV4RRFFQ69G5FB6"
	rec := executeRecord(`{}`)
	rec.Definition.Action = cron.Action{Kind: "notify", Recipient: "alice", Message: "hi"}
	rec.Definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: wasTarget}
	m, api := openEdit(t, rec)
	m.cron.form.val[cfIssue] = newTarget
	theirs := rec
	theirs.Definition.Trigger = cron.Trigger{Kind: "issue-scheduled", IssueUID: source}
	m = conflictAndApply(t, m, api, theirs)
	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	got := api.puts[1].write.Definition
	require.Equal(t, newTarget, got.Issue.UID, "the edited target stays the target")
	require.Equal(t, source, got.Trigger.IssueUID)
}

func TestCronEditKeepsMarkdownTextVerbatim(t *testing.T) {
	rec := executeRecord(`{}`)
	rec.Definition.Action.Prompt = "  - indented\n    more\n"
	rec.Definition.Issue = &cron.IssuePolicy{Kind: "per-run", Title: " Title ", Body: "    code\n"}
	m, api := openEdit(t, rec)
	m.cron.form.val[cfName] = "Renamed"
	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	got := api.puts[0].write.Definition
	require.Equal(t, rec.Definition.Action.Prompt, got.Action.Prompt)
	require.Equal(t, rec.Definition.Issue.Title, got.Issue.Title)
	require.Equal(t, rec.Definition.Issue.Body, got.Issue.Body)

	note := executeRecord(`{}`)
	note.Definition.Action = cron.Action{Kind: "notify", Recipient: "alice", Message: "line\n\n"}
	note.Definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: cronTestIssue}
	m, api = openEdit(t, note)
	m.cron.form.val[cfName] = "Renamed"
	m, cmd = press(m, ctrlKey('o'))
	settle(t, m, cmd)
	require.Equal(t, "line\n\n", api.puts[0].write.Definition.Action.Message)
}

func TestCronOtherConflictsAreOrdinarySaveErrors(t *testing.T) {
	for _, create := range []bool{false, true} {
		m, api := cronWriteFixture(t)
		if create {
			m, _ = press(m, keyRune('n'))
			m.cron.form.val[cfName], m.cron.form.val[cfParent], m.cron.form.val[cfTitle] = "W", "abc4", "R"
		} else {
			m, api = openEdit(t, executeRecord(`{}`))
		}
		api.shown = executeRecord(`{}`)
		api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "federated_read_only", Message: "write on the owning hub"}}
		m, cmd := press(m, ctrlKey('o'))
		m = settle(t, m, cmd)
		require.Nil(t, m.cron.form.conflict, "create=%v", create)
		require.Contains(t, m.cron.form.general, "owning hub")
	}
}

func TestCronRetriedCreateThatAlreadyLanded(t *testing.T) {
	m, api := cronWriteFixture(t)
	m, _ = press(m, keyRune('n'))
	f := m.cron.form
	f.val[cfName], f.val[cfParent], f.val[cfTitle] = "Weekly report", "abc4", "Report"
	name, def, err := f.build(cronUIDResolver(map[string]string{"abc4": cronTestIssue}))
	require.NoError(t, err)
	api.shown = CronJobRecord{UID: f.uid, Name: name, Definition: def, DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB8"}
	exists := &APIError{Status: http.StatusConflict, Code: "cron_conflict"}
	api.putErrs = []error{exists}

	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form, "the stored job is the one that was sent, so the create is done")
	require.Equal(t, "Created Weekly report", m.toast.text)

	m, _ = press(m, keyRune('n'))
	m.cron.form.val[cfName], m.cron.form.val[cfParent], m.cron.form.val[cfTitle] = "Weekly report", "abc4", "Report"
	api.shown.Definition.Issue.Title = "Someone else's"
	api.putErrs = []error{exists}
	m, cmd = press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.NotNil(t, m.cron.form.conflict, "a different job under the UID is shown, not hidden")
}

func TestCronEditReplyIsRefusedWhileFiltering(t *testing.T) {
	m, api := cronWriteFixture(t)
	api.shown = executeRecord(`{}`)
	api.shown.UID = m.cron.data.jobs[0].UID
	m, cmd := press(m, keyRune('e'))
	m, _ = press(m, keyRune('/'))
	require.True(t, m.cron.filtering)
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
}

func TestCronJobKeysWorkOnADetailReachedFromAnotherTab(t *testing.T) {
	m, _ := cronWriteFixture(t)
	m = sendRune(m, '3')
	m = sendKey(m, tea.KeyEnter)
	_, _, lines := cronScreen(m)
	for i, l := range lines {
		if l.target.kind == cronJobs {
			for range i {
				m = sendRune(m, 'j')
			}
			break
		}
	}
	m = sendKey(m, tea.KeyEnter)
	require.Equal(t, cronJobs, m.cron.stack[len(m.cron.stack)-1].ref.kind)
	m, _ = press(m, keyRune('d'))
	require.NotNil(t, m.cron.confirm, "the job's page offers its actions whichever tab led there")
}

func TestCronArchiveFromDetailReturnsToTheList(t *testing.T) {
	m, api := cronWriteFixture(t)
	m = sendKey(m, tea.KeyEnter)
	require.Len(t, m.cron.stack, 1)
	m, _ = press(m, keyRune('d'))
	m, cmd := press(m, keyRune('y'))
	m = settle(t, m, cmd)
	require.Len(t, api.archives, 1)
	require.Empty(t, m.cron.stack, "the archived job's page would be empty, so the list is shown")
	require.Equal(t, "Archived Daily summary; press a, then u to restore it", m.toast.text)
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
	require.Contains(t, m.toast.text, "Archived Daily summary")

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
	err = c.PutCronJob(t.Context(), project.ID, jobUID, CronJobWrite{Name: "Digest", Actor: "alice", Definition: def})
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

func TestCronEditRefusesAnArchivedRecord(t *testing.T) {
	m, api := cronWriteFixture(t)
	api.shown = executeRecord(`{}`)
	api.shown.UID = m.cron.data.jobs[0].UID
	api.shown.Archived = true
	m, cmd := press(m, keyRune('e'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form, "saving an archived job's revision would restore it")
	require.Contains(t, m.toast.text, "restore")
}

func TestCronEditResponseNeedsItsContext(t *testing.T) {
	for name, away := range map[string]func(Model) Model{
		"help":      func(m Model) Model { m, _ = updateModel(m, keyRune('?')); return m },
		"other tab": func(m Model) Model { return sendRune(m, '3') },
	} {
		t.Run(name, func(t *testing.T) {
			m, api := cronWriteFixture(t)
			api.shown = executeRecord(`{}`)
			api.shown.UID = m.cron.data.jobs[0].UID
			m, cmd := press(m, keyRune('e'))
			m = away(m)
			m = settle(t, m, cmd)
			require.Nil(t, m.cron.form)
		})
	}
}

func TestCronFormTakesPaste(t *testing.T) {
	m, _ := cronWriteFixture(t)
	m, _ = updateModel(m, keyRune('n'))
	m, _ = updateModel(m, tea.PasteMsg{Content: "pasted name"})
	require.Equal(t, "pasted name", m.cron.form.ed.Value())
	require.True(t, m.cron.form.dirty())
	require.Equal(t, "pasted name", m.cron.form.val[cfName])

	m.cron.form.focusField(cfKind) // a choice field takes no text
	m, _ = updateModel(m, tea.PasteMsg{Content: "zzz"})
	require.Equal(t, "pasted name", m.cron.form.val[cfName])
	require.Equal(t, "recurring", m.cron.form.kind)
}

func TestCronEditShowsTheReasonWhenTheJobCannotBeRead(t *testing.T) {
	m, api := cronWriteFixture(t)
	api.showErr = &APIError{Status: http.StatusNotFound, Code: "not_found", Message: "cron not found"}
	m, cmd := press(m, keyRune('e'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
	require.Contains(t, m.toast.text, "Could not open the job")
}

func TestCronLateArchiveReplyClosesOnlyItsOwnPage(t *testing.T) {
	m, _ := cronWriteFixture(t)
	second := m.cron.data.jobs[0]
	second.UID, second.Name, second.ID = "01ARZ3NDEKTSV4RRFFQ69G5FB0", "Weekly digest", 5
	m.cron.data.jobs = append(m.cron.data.jobs, second)
	m = sendKey(m, tea.KeyEnter)
	m, _ = press(m, keyRune('d'))
	m, cmd := press(m, keyRune('y'))
	m = sendRune(m, 'h')
	m = sendRune(m, 'j')
	m = sendKey(m, tea.KeyEnter)
	require.Equal(t, second.UID, m.cron.stack[0].ref.uid)
	m = settle(t, m, cmd)
	require.Len(t, m.cron.stack, 1, "the reply is about the first job; the second job's page stays")
	require.Equal(t, second.UID, m.cron.stack[0].ref.uid)
}

func TestCronArchiveHintOnlyWhereTheKeysWork(t *testing.T) {
	m, _ := cronWriteFixture(t)
	m = sendRune(m, '4')
	m = sendKey(m, tea.KeyEnter)
	_, _, lines := cronScreen(m)
	for i, l := range lines {
		if l.target.kind == cronJobs {
			for range i {
				m = sendRune(m, 'j')
			}
			break
		}
	}
	m = sendKey(m, tea.KeyEnter)
	m, _ = press(m, keyRune('d'))
	m, cmd := press(m, keyRune('y'))
	m = settle(t, m, cmd)
	require.Equal(t, "Archived Daily summary", m.toast.text, "a does nothing on the workflow below")
}

func TestCronRetriedCreateOfAnArchivedJobSaysSo(t *testing.T) {
	m, api := cronWriteFixture(t)
	m, _ = press(m, keyRune('n'))
	f := m.cron.form
	f.val[cfName], f.val[cfParent], f.val[cfTitle] = "Weekly report", "abc4", "Report"
	name, def, err := f.build(cronUIDResolver(map[string]string{"abc4": cronTestIssue}))
	require.NoError(t, err)
	api.shown = CronJobRecord{UID: f.uid, Name: name, Definition: def, Archived: true}
	api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "cron_conflict"}}
	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.NotNil(t, m.cron.form)
	require.Nil(t, m.cron.form.conflict)
	require.Contains(t, m.cron.form.general, "restore it with u")
}
