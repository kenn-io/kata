package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	katauid "go.kenn.io/kata/internal/uid"
)

type cronPut struct {
	uid   string
	write kataapi.PutCronJobBody
}

// cronWriteTestAPI doubles the cron write boundary; reads come from the fixture.
type cronWriteTestAPI struct {
	*cronReadTestAPI
	puts      []cronPut
	putErrs   []error // consumed one per put
	shown     db.CronJob
	showErr   error
	issueUIDs map[string]string
}

func (a *cronWriteTestAPI) ShowCronJob(context.Context, int64, string) (db.CronJob, error) {
	return a.shown, a.showErr
}

func (a *cronWriteTestAPI) PutCronJob(_ context.Context, _ int64, uid string, w kataapi.PutCronJobBody) error {
	a.puts = append(a.puts, cronPut{uid, w})
	if len(a.putErrs) > 0 {
		err := a.putErrs[0]
		a.putErrs = a.putErrs[1:]
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "cron_conflict" {
			raw, marshalErr := json.Marshal(struct {
				Current kataapi.CronJob `json:"current"`
			}{kataapi.CronJobFrom(a.shown)})
			if marshalErr != nil {
				return marshalErr
			}
			apiErr.Data = raw
		}
		return err
	}
	return nil
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

	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.NotNil(t, m.cron.form, "a failed save keeps the draft")
	require.Contains(t, m.cron.form.general, "connection reset")

	m, cmd = press(m, ctrlKey('o'))
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
	m, cmd := press(m, ctrlKey('o'))
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
	api.showErr = errors.New("current job must come from the conflict response")
	api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "cron_conflict"}, nil}

	m, cmd = press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Equal(t, opened, api.puts[0].write.ExpectedEventUID, "the edit is guarded on the event it read")
	require.NotNil(t, m.cron.form.conflict)
	require.Equal(t, "My edit", m.cron.form.val[cfTitle], "the draft survives the conflict")
	view := stripANSI(m.viewBody())
	require.Contains(t, view, "01ARZ3NDEKTSV4RRFFQ69G5FB8", "the current job is shown")
	require.Contains(t, view, "save my draft")

	m, cmd = press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
	require.Len(t, api.puts, 2)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FB8", api.puts[1].write.ExpectedEventUID, "the draft is applied to the current definition")
	require.Equal(t, "My edit", api.puts[1].write.Definition.Issue.Title)
	require.True(t, api.puts[1].write.Definition.Enabled, "saving replaces the whole definition with the kept draft")
}

func TestCronConflictRefusesFutureVersion(t *testing.T) {
	m, api := openEdit(t, executeRecord(`{}`))
	m.cron.form.val[cfTitle] = "My draft"
	opened := m.cron.form.expected
	api.shown.Definition.Version = 2
	api.shown.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB8"
	api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "cron_conflict"}}
	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.NotNil(t, m.cron.form)
	require.Nil(t, m.cron.form.conflict, "an unsupported definition cannot authorize replacement")
	require.Contains(t, m.cron.form.general, "version")
	require.Equal(t, "My draft", m.cron.form.val[cfTitle])
	require.Equal(t, opened, m.cron.form.expected)
	require.Len(t, api.puts, 1)
}

func TestCronJobKeysRespectMutationAuthority(t *testing.T) {
	for _, key := range []rune{'n', 'e'} {
		for _, pending := range []bool{false, true} {
			m, _ := cronWriteFixture(t)
			m.issueScoped, m.scopedWritable = !pending, true
			m.authCapabilitiesRequired, m.authCapabilitiesReady = pending, false
			m, cmd := updateModel(m, keyRune(key))
			require.Nil(t, m.cron.form, "key=%c pending=%v", key, pending)
			require.Nil(t, cmd, "key=%c pending=%v", key, pending)
		}
	}
}

func TestCronJobKeysRetryPermissionDiscovery(t *testing.T) {
	for _, key := range []rune{'n', 'e'} {
		t.Run(string(key), func(t *testing.T) {
			m, _ := cronWriteFixture(t)
			m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{err: errors.New("temporary connection failure")})
			m, cmd := updateModel(m, keyRune(key))
			require.Nil(t, m.cron.form)
			require.NotNil(t, cmd, "retrying a job action must retry permission discovery")
		})
	}
}

func TestCronSaveDoesNotShareEditorWithRender(t *testing.T) {
	m, api := openEdit(t, executeRecord(`{}`))
	m.cron.form.ed.activeField().input.SetValue("Renamed digest")
	m, cmd := press(m, ctrlKey('o'))
	require.NotNil(t, cmd)
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	for range 100 {
		cronFormLines(m, m.cron.form)
	}
	msg := (<-done).(cronSavedMsg)
	require.NoError(t, msg.err)
	require.Len(t, api.puts, 1)
	require.Equal(t, "Renamed digest", api.puts[0].write.Name)
}

func openEdit(t *testing.T, record db.CronJob) (Model, *cronWriteTestAPI) {
	t.Helper()
	m, api := cronWriteFixture(t)
	api.shown = record
	api.shown.UID = m.cron.data.jobs[0].UID
	m, cmd := press(m, keyRune('e'))
	return settle(t, m, cmd), api
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

func TestCronEditDefinitionRoundTripsThroughDaemon(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	project, err := c.EnsureProject(t.Context(), "cron-ui-example", "alice")
	require.NoError(t, err)
	const options = `{"future":{"keep":[1,2]},"big":12345678901234567890123,"exact":1.10}`
	const jobUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	def := manualExecuteJob(options)
	def.Action.Prompt = "Line one\nLine two"
	err = c.PutCronJob(t.Context(), project.ID, jobUID, kataapi.PutCronJobBody{Name: "Digest", Actor: "alice", Definition: kataapi.CronJobDefinitionFrom(def)})
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
	m, cmd = press(m, ctrlKey('o'))
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
	api.shown.DeletedAt = new(time.Now())
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
	require.Equal(t, "pasted name", m.cron.form.ed.activeField().value())
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

func TestCronEditOfAJobWithoutAZoneWritesNone(t *testing.T) {
	rec := executeRecord(`{}`)
	rec.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 7 * * *"}
	m, api := openEdit(t, rec)
	m.cron.form.val[cfName] = "Renamed"
	m, cmd := press(m, ctrlKey('o'))
	settle(t, m, cmd)
	require.Empty(t, api.puts[0].write.Definition.Trigger.Timezone)
}

func TestCronEditCanClearTheTimeZone(t *testing.T) {
	rec := executeRecord(`{}`)
	rec.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 7 * * *", Timezone: "Europe/Berlin"}
	m, api := openEdit(t, rec)
	m.cron.form.val[cfZone] = ""
	m, cmd := press(m, ctrlKey('o'))
	settle(t, m, cmd)
	require.Empty(t, api.puts[0].write.Definition.Trigger.Timezone)
}

func TestCronLongFieldKeepsCursorVisible(t *testing.T) {
	m, _ := openEdit(t, executeRecord(`{}`))
	m.width = 60
	m.cron.form.val[cfName] = strings.Repeat("x", 100)
	m.cron.form.focusField(cfName)
	m, _ = updateModel(m, tea.PasteMsg{Content: "TAILMARK"})
	require.Contains(t, stripANSI(m.viewBody()), "TAILMARK")
}

func TestCronScopeResetClosesDiscardModal(t *testing.T) {
	m, _ := openEdit(t, executeRecord(`{}`))
	m.cron.form.val[cfTitle] = "changed"
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, modalDiscardCronJob, m.modal)
	m.scopeGen++
	m, _ = m.cronSync()
	require.Equal(t, modalNone, m.modal)
}

func TestCronConflictReloadUsesCurrentJob(t *testing.T) {
	m, api := openEdit(t, executeRecord(`{}`))
	m.cron.form.val[cfTitle] = "My draft"
	api.shown.Name = "Current name"
	api.shown.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB8"
	api.shown.Definition.Action.Prompt = "Current prompt"
	api.shown.Definition.Options = []byte(`{"current_option":12345678901234567890}`)
	api.putErrs = []error{&APIError{Status: http.StatusConflict, Code: "cron_conflict"}}
	m, cmd := press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Equal(t, "My draft", m.cron.form.val[cfTitle])
	var viewed strings.Builder
	for range 60 {
		viewed.WriteString(stripANSI(m.viewBody()))
		m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	require.Contains(t, viewed.String(), "Current prompt")
	require.Contains(t, viewed.String(), "12345678901234567890")
	m, cmd = press(m, keyRune('r'))
	require.Nil(t, cmd)
	require.Len(t, api.puts, 1, "reload makes no write")
	require.Nil(t, m.cron.form.conflict)
	require.False(t, m.cron.form.dirty())
	require.Equal(t, "Current name", m.cron.form.val[cfName])
	require.Equal(t, "Current prompt", m.cron.form.val[cfPrompt])
	require.Equal(t, api.shown.DefinitionEventUID, m.cron.form.expected)
}

func TestCronCreateRetryKeepsConflictOnDaemon(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	project, err := c.EnsureProject(t.Context(), "cron-retry-example", "alice")
	require.NoError(t, err)
	m := newTestModel()
	m.api, m.scope, m.list.actor = c, scope{projectID: project.ID}, "alice"
	f := newNativeForm(t)
	parent, err := c.CreateIssue(t.Context(), project.ID, CreateIssueBody{Title: "Report parent", Actor: "alice"})
	require.NoError(t, err)
	f.val[cfParent] = parent.Issue.UID
	m.cron.form = f
	name, def, err := f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	// The daemon committed the first send, but the form still has its draft.
	require.NoError(t, c.PutCronJob(t.Context(), project.ID, f.uid, kataapi.PutCronJobBody{Name: name, Definition: kataapi.CronJobDefinitionFrom(def), Actor: "alice"}))
	m = settle(t, m, m.cronSaveCmd(*f))
	require.NotNil(t, m.cron.form)
	require.NotNil(t, m.cron.form.conflict)
	require.Equal(t, f.uid, m.cron.form.conflict.UID)
	jobs, err := c.ListCronJobs(t.Context(), project.ID)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, f.uid, jobs[0].UID)
}

func TestCronTextFieldsUseExternalEditorWithoutChangingStoredText(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	for _, field := range []cronField{cfBody, cfPrompt, cfMessage, cfOptions} {
		t.Run(cronFieldLabels[field], func(t *testing.T) {
			rec := executeRecord(" {\n\t\"exact\": 1.10\r\n} ")
			original := "make:\r\n\tgo test ./...\n"
			if field == cfBody || field == cfPrompt {
				original += strings.Repeat("x\n", 10005)
			}
			rec.Definition.Issue.Body, rec.Definition.Action.Prompt = original, original
			if field == cfMessage {
				rec.Definition.Action = cron.Action{Kind: "notify", Recipient: "alice", Message: original}
				rec.Definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: cronTestIssue}
			}
			if field == cfOptions {
				original = string(rec.Definition.Options)
			}
			m, _ := openEdit(t, rec)
			m.cron.form.focusField(field)
			require.Equal(t, original, m.cron.form.val[field])
			m, cmd := updateModel(m, ctrlKey('e'))
			require.NotNil(t, cmd, "long text uses the existing editor handoff")
			// The editor return carries exact bytes, including content beyond the textarea's limits.
			edited := original + strings.Repeat("x\n", 10005)
			switch field {
			case cfMessage:
				edited = original + "changed\n"
			case cfOptions:
				edited = " {\n\t\"exact\": 1.10,\r\n\"more\": true\n} "
			}
			m, _ = updateModel(m, editorReturnedMsg{formGen: m.nextFormGen, content: edited})
			require.Equal(t, edited, m.cron.form.val[field])
			_, def, err := m.cron.form.build(cronUIDResolver(nil))
			require.NoError(t, err)
			got := def.Action.Prompt
			switch field {
			case cfBody:
				got = def.Issue.Body
			case cfMessage:
				got = def.Action.Message
			case cfOptions:
				got = string(def.Options)
			}
			require.Equal(t, edited, got)
		})
	}
}

func TestCronNativeCreateRefreshesParentActivity(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	project, err := c.EnsureProject(t.Context(), "cron-parent-example", "alice")
	require.NoError(t, err)
	parent, err := c.CreateIssue(t.Context(), project.ID, CreateIssueBody{Title: "Parent", Actor: "alice"})
	require.NoError(t, err)
	m := newTestModel()
	m.api, m.scope, m.list.actor = c, scope{projectID: project.ID}, "alice"
	m.detail.scopePID = project.ID
	m.detail.issue = parent.Issue
	for _, msg := range runCmdTree(m.detail.refetch(c)) {
		m.detail, _ = m.detail.Update(msg, m.keymap, c)
	}
	before := len(m.detail.events)
	f := newNativeForm(t)
	f.val[cfParent] = parent.Issue.UID
	m.cron.form = f
	msg := m.cronSaveCmd(*f)().(cronSavedMsg)
	require.NoError(t, msg.err)
	m, cmd := m.cronSaved(msg)
	for _, msg := range runCmdTree(cmd) {
		m.detail, _ = m.detail.Update(msg, m.keymap, c)
	}
	require.Greater(t, len(m.detail.events), before)
}

func TestCronValidationMessagesRemainVisible(t *testing.T) {
	for _, tc := range []struct {
		field cronField
		value string
	}{
		{cfCron, "invalid"},
		{cfOptions, "[1]"},
		{cfDeadline, "9223372036854775807"},
	} {
		t.Run(cronFieldLabels[tc.field], func(t *testing.T) {
			rec := executeRecord(`{}`)
			rec.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 * * *"}
			m, _ := openEdit(t, rec)
			m.width = 60
			m.cron.form.val[tc.field] = tc.value
			m.cron.form.focusField(tc.field)
			_, _, err := m.cron.form.build(cronUIDResolver(nil))
			require.Error(t, err)
			m, cmd := press(m, ctrlKey('o'))
			require.Nil(t, cmd)
			require.Contains(t, strings.Join(strings.Fields(stripANSI(m.viewBody())), " "), err.Error())
		})
	}
}

func TestCronEditorReturnKeepsDraftOnErrorAndIgnoresClosedForm(t *testing.T) {
	m, _ := openEdit(t, executeRecord(`{}`))
	m.cron.form.focusField(cfPrompt)
	original, gen := m.cron.form.val[cfPrompt], m.nextFormGen
	m, _ = updateModel(m, editorReturnedMsg{formGen: gen, err: errors.New("editor failed")})
	require.Equal(t, original, m.cron.form.val[cfPrompt])
	require.Contains(t, m.cron.form.general, "editor failed")
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	m, _ = press(m, keyRune('n'))
	m.cron.form.focusField(cfBody)
	m, _ = updateModel(m, editorReturnedMsg{formGen: gen, content: "stale text"})
	require.Empty(t, m.cron.form.val[cfBody])
}
