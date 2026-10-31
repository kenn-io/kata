package tui

import (
	"testing"

	"github.com/stretchr/testify/require"
	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

const (
	cronTestParent = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	cronTestIssue  = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
)

func cronTestUIDs(string) string { return cronTestIssue }

func newNativeForm(t *testing.T) *cronForm {
	t.Helper()
	f, err := newCronForm(nil)
	require.NoError(t, err)
	f.val[cfName], f.val[cfParent], f.val[cfTitle] = "Weekly report", cronTestParent, "Report"
	return f
}

func nativeRecord() db.CronJob {
	return db.CronJob{
		UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "Weekly report", DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB2",
		Definition: cron.JobDefinition{
			Version: 1, Kind: "job", Enabled: true,
			Trigger: cron.Trigger{Kind: "cron", Cron: "0 9 * * 1", Timezone: "UTC"},
			Action:  cron.Action{Kind: "notify", Recipient: "current-owner-or-author"},
			Issue:   &cron.IssuePolicy{Kind: "per-run", ParentUID: cronTestParent, Title: "Report"},
			Overlap: "allow", Catchup: "latest",
		},
	}
}

func executeRecord(options string) db.CronJob {
	return db.CronJob{
		UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "Digest", DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB2",
		Definition: manualExecuteJob(options),
	}
}

func TestCronFormRecurringCreateHonoursEnabled(t *testing.T) {
	f := newNativeForm(t)
	_, def, err := f.build(cronTestUIDs)
	require.NoError(t, err)
	require.True(t, def.Enabled, "a new job starts enabled")

	f.step(cfEnabled, 1)
	_, def, err = f.build(cronTestUIDs)
	require.NoError(t, err)
	require.False(t, def.Enabled)
}

func TestCronFormPolicySwitchDropsOffsets(t *testing.T) {
	f, err := cronFormFor(executeRecord(`{}`), nil)
	require.NoError(t, err)
	_, def, err := f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.NotNil(t, def.Issue.ScheduledOffsetSeconds, "per-run keeps its offset")

	f.val[cfPolicy], f.val[cfReport] = "existing", cronTestIssue
	_, def, err = f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Equal(t, cron.IssuePolicy{Kind: "existing", UID: cronTestIssue}, *def.Issue)
}

func TestCronFormEditSavesWhenAnotherJobSharesTheName(t *testing.T) {
	m, api := cronWriteFixture(t)
	twin := m.cron.data.jobs[0]
	twin.UID, twin.ID = "01ARZ3NDEKTSV4RRFFQ69G5FAZ", 9
	m.cron.data.jobs = append(m.cron.data.jobs, twin)
	api.shown = executeRecord(`{}`)
	api.shown.Name = twin.Name
	api.shown.UID = m.cron.data.jobs[0].UID
	m, cmd := press(m, keyRune('e'))
	m = settle(t, m, cmd)
	m.cron.form.val[cfTitle] = "Edited"
	m, cmd = press(m, ctrlKey('o'))
	m = settle(t, m, cmd)
	require.Nil(t, m.cron.form)
	require.Len(t, api.puts, 1)
	require.Equal(t, twin.Name, api.puts[0].write.Name)
}

func TestCronFormExecuteDateTriggerUsesTheTriggerIssueForTheRun(t *testing.T) {
	f := newNativeForm(t)
	f.kind, f.trigger = "prompt", "issue-scheduled"
	f.val[cfPrompt], f.val[cfPolicy], f.val[cfIssue], f.val[cfReport] = "Do it", "existing", cronTestParent, cronTestIssue
	require.False(t, f.shown(cfReport), "the run issue is the trigger's issue, so there is nothing to ask")
	_, def, err := f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Equal(t, cronTestParent, def.Trigger.IssueUID)
	require.Equal(t, cronTestParent, def.Issue.UID)

	f.trigger = "manual"
	require.True(t, f.shown(cfReport))
}

func TestCronFormExecuteDateTriggerCreatesOnTheDaemon(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	project, err := c.EnsureProject(t.Context(), "cron-date-example", "alice")
	require.NoError(t, err)
	created, err := c.CreateIssue(t.Context(), project.ID, CreateIssueBody{Title: "Dated", Actor: "alice"})
	require.NoError(t, err)
	issue := created.Issue.UID

	f := newNativeForm(t)
	f.kind, f.trigger = "prompt", "issue-scheduled"
	f.val[cfPrompt], f.val[cfPolicy], f.val[cfIssue] = "Do it", "existing", issue
	name, def, err := f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.NoError(t, c.PutCronJob(t.Context(), project.ID, f.uid, kataapi.PutCronJobBody{Name: name, Definition: kataapi.CronJobDefinitionFrom(def), Actor: "alice"}))
	shown, err := c.ShowCronJob(t.Context(), project.ID, f.uid)
	require.NoError(t, err)
	require.Equal(t, issue, shown.Definition.Trigger.IssueUID)
	require.Equal(t, issue, shown.Definition.Issue.UID)
}

func TestCronFormLocksNativeParentAndAction(t *testing.T) {
	f, err := cronFormFor(nativeRecord(), nil)
	require.NoError(t, err)
	require.True(t, f.locked(cfParent))
	require.Len(t, f.kinds(), 1, "a native job cannot become another kind")
	require.Equal(t, cronRecurringID, f.kinds()[0].id)

	f.focusField(cfName)
	for range 30 {
		f.move(1)
		require.NotEqual(t, cfParent, f.focus, "tab skips the locked parent")
	}
	f.step(cfKind, 1)
	require.Equal(t, cronRecurringID, f.kind)

	f.val[cfParent] = "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	_, def, err := f.build(cronTestUIDs)
	require.NoError(t, err)
	require.Equal(t, cronTestParent, def.Issue.ParentUID)
	require.Equal(t, "notify", def.Action.Kind)

	created := newNativeForm(t)
	require.False(t, created.locked(cfParent))
}

func TestCronFormAdapterJobsCanBecomeNative(t *testing.T) {
	f, err := cronFormFor(executeRecord(`{}`), nil)
	require.NoError(t, err)
	require.Equal(t, cronRecurringID, f.kinds()[0].id)
	f.step(cfKind, -1)
	require.Equal(t, cronRecurringID, f.kind)
	f.val[cfParent] = cronTestParent
	_, def, err := f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.True(t, cron.IsRecurringIssueJob(def))
	require.Equal(t, cronTestParent, def.Issue.ParentUID)
}

func TestCronFormNotifyIssueTargetIsExisting(t *testing.T) {
	f, err := newCronForm(nil)
	require.NoError(t, err)
	f.kind, f.trigger = "notify", "manual"
	f.val[cfName], f.val[cfIssue], f.val[cfMessage] = "Ping", cronTestIssue, "hello"
	f.val[cfRecipient] = "alice"
	_, def, err := f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Equal(t, cron.IssuePolicy{Kind: "existing", UID: cronTestIssue}, *def.Issue)

	f.trigger = "issue-deadline"
	f.val[cfReport] = cronTestParent
	_, def, err = f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Equal(t, cron.IssuePolicy{Kind: "existing", UID: cronTestParent}, *def.Issue, "the notification target is its own field")
	require.Equal(t, "issue-deadline", def.Trigger.Kind)
	require.Equal(t, cronTestIssue, def.Trigger.IssueUID)

	f.val[cfReport] = ""
	_, def, err = f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Nil(t, def.Issue, "a date-triggered notify may leave its issue to the trigger")
}

func TestCronFormOptionsAreKeptVerbatim(t *testing.T) {
	const options = `{"future":{"keep":[1,2]},"big":12345678901234567890123,"exact":1.10}`
	f, err := cronFormFor(executeRecord(options), nil)
	require.NoError(t, err)
	for range 40 {
		f.move(1) // tabbing through every field must not rewrite any of them
	}
	f.val[cfTitle] = "Digest 2"
	_, def, err := f.build(cronTestUIDs)
	require.NoError(t, err)
	require.Equal(t, options, string(def.Options))
	require.Contains(t, string(def.Options), "12345678901234567890123")
	require.Contains(t, string(def.Options), "1.10")
	require.Equal(t, "Line one\nLine two", def.Action.Prompt, "line breaks survive a pass through the editor")
}

func TestCronFormEveryKindAndTrigger(t *testing.T) {
	for _, tc := range []struct {
		kind, trigger string
		set           map[cronField]string
	}{
		{"recurring", "cron", nil},
		{"recurring", "interval", map[cronField]string{cfInterval: "90"}},
		{"prompt", "cron", map[cronField]string{cfPrompt: "Do it", cfTitle: "T"}},
		{"prompt", "interval", map[cronField]string{cfPrompt: "Do it", cfTitle: "T", cfInterval: "30s"}},
		{"prompt", "once", map[cronField]string{cfPrompt: "Do it", cfTitle: "T", cfAt: "2030-01-02T03:04:05Z"}},
		{"prompt", "manual", map[cronField]string{cfPrompt: "Do it", cfTitle: "T"}},
		{"prompt", "issue-scheduled", map[cronField]string{cfPrompt: "Do it", cfTitle: "T", cfIssue: "abc4"}},
		{"workflow", "manual", map[cronField]string{cfWorkflow: cronTestParent, cfTitle: "T"}},
		{"notify", "cron", map[cronField]string{cfIssue: "abc4", cfMessage: "m", cfRecipient: "alice"}},
		{"notify", "issue-scheduled", map[cronField]string{cfMessage: "m", cfRecipient: "alice", cfIssue: "abc4"}},
		{"notify", "issue-deadline", map[cronField]string{cfMessage: "m", cfRecipient: "alice", cfIssue: "abc4", cfLead: "2h"}},
	} {
		t.Run(tc.kind+"/"+tc.trigger, func(t *testing.T) {
			f := newNativeForm(t)
			f.kind, f.trigger = tc.kind, tc.trigger
			for field, v := range tc.set {
				f.val[field] = v
			}
			_, def, err := f.build(cronTestUIDs)
			require.NoError(t, err)
			require.Equal(t, tc.trigger, def.Trigger.Kind)
		})
	}
}

func TestCronFormNotifyEditKeepsStoredOverlap(t *testing.T) {
	rec := executeRecord(`{}`)
	rec.Definition.Action = cron.Action{Kind: "notify", Recipient: "alice", Message: "hi"}
	rec.Definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: cronTestIssue}
	rec.Definition.Overlap = "forbid"
	f, err := cronFormFor(rec, nil)
	require.NoError(t, err)
	f.val[cfMessage] = "changed"
	_, def, err := f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Equal(t, "forbid", def.Overlap)

	created, err := newCronForm(nil)
	require.NoError(t, err)
	created.kind, created.trigger = "notify", "manual"
	created.val[cfIssue], created.val[cfRecipient], created.val[cfMessage] = cronTestIssue, "alice", "hi"
	_, def, err = created.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Equal(t, "allow", def.Overlap, "a new notify job gets a default")
}

func TestCronFormRefusesFutureVersion(t *testing.T) {
	rec := executeRecord(`{}`)
	rec.Definition.Version = 2
	_, err := cronFormFor(rec, nil)
	require.Error(t, err)
}

func TestCronFormCatchupLatestClearsGrace(t *testing.T) {
	rec := nativeRecord()
	rec.Definition.Catchup, rec.Definition.GraceSeconds = "skip", 300
	f, err := cronFormFor(rec, nil)
	require.NoError(t, err)
	f.step(cfCatchup, 1)
	_, def, err := f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Zero(t, def.GraceSeconds)

	created := newNativeForm(t)
	created.step(cfCatchup, -1)
	created.val[cfGrace] = "300"
	created.step(cfCatchup, 1)
	_, def, err = created.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Zero(t, def.GraceSeconds)
}

func TestCronFormRenameKeepsStoredCronText(t *testing.T) {
	rec := nativeRecord()
	rec.Definition.Trigger.Cron = "0 9 * * 1\t"
	f, err := cronFormFor(rec, nil)
	require.NoError(t, err)
	f.ed.activeField().input.SetValue("Renamed report")
	name, def, err := f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Equal(t, "Renamed report", name)
	require.Equal(t, "0 9 * * 1\t", def.Trigger.Cron)
}

func TestCronFormRenameKeepsStoredGrace(t *testing.T) {
	for _, catchup := range []string{"latest", "all"} {
		t.Run(catchup, func(t *testing.T) {
			rec := nativeRecord()
			rec.Definition.Catchup, rec.Definition.GraceSeconds = catchup, 30
			f, err := cronFormFor(rec, nil)
			require.NoError(t, err)
			f.ed.activeField().input.SetValue("Renamed report")
			name, def, err := f.build(cronUIDResolver(nil))
			require.NoError(t, err)
			require.Equal(t, "Renamed report", name)
			require.Equal(t, int64(30), def.GraceSeconds)
		})
	}
}

func TestCronFormHiddenGraceDoesNotBlockSave(t *testing.T) {
	rec := executeRecord(`{}`)
	rec.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 * * *"}
	f, err := cronFormFor(rec, nil)
	require.NoError(t, err)
	f.step(cfCatchup, -1) // latest to skip
	f.val[cfGrace] = "invalid grace"
	f.validate()
	require.NotNil(t, f.err)
	require.Equal(t, cfGrace, f.err.field)
	for range 3 {
		f.step(cfTrigger, 1) // cron through interval and once to manual
	}
	require.False(t, f.shown(cfGrace))
	_, def, err := f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Equal(t, "manual", def.Trigger.Kind)
	require.Zero(t, def.GraceSeconds)
}

func TestCronFormHiddenCatchupKeepsGrace(t *testing.T) {
	for _, catchup := range []string{"skip", "latest", "all"} {
		t.Run(catchup, func(t *testing.T) {
			rec := executeRecord(`{}`)
			rec.Definition.Action = cron.Action{Kind: "notify", Recipient: "alice", Message: "hello"}
			rec.Definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: cronTestIssue}
			rec.Definition.Trigger = cron.Trigger{Kind: "issue-deadline", IssueUID: cronTestIssue}
			rec.Definition.Catchup, rec.Definition.GraceSeconds = catchup, 10
			f, err := cronFormFor(rec, nil)
			require.NoError(t, err)
			_, def, err := f.build(cronUIDResolver(nil))
			require.NoError(t, err)
			require.Equal(t, int64(10), def.GraceSeconds)
		})
	}
}

func TestCronFormEmptyWorkflowStepsToFirstOrLast(t *testing.T) {
	for _, delta := range []int{1, -1} {
		f := newNativeForm(t)
		f.kind = "workflow"
		f.workflows = []cronChoice{{cronTestIssue, "First"}, {cronTestParent, "Last"}}
		f.step(cfWorkflow, delta)
		want := cronTestIssue
		if delta < 0 {
			want = cronTestParent
		}
		require.Equal(t, want, f.val[cfWorkflow])
	}
}
