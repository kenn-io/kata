package tui

import (
	"testing"

	"github.com/stretchr/testify/require"
	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
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

func nativeRecord() CronJobRecord {
	return CronJobRecord{
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

func executeRecord(options string) CronJobRecord {
	return CronJobRecord{
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

func TestCronFormPlacesCronMessagesOnTheBlankField(t *testing.T) {
	f := newNativeForm(t)
	f.val[cfParent] = ""
	f.validate()
	require.NotNil(t, f.err)
	require.Equal(t, cfParent, f.err.field, "the title is filled; the blank parent is blamed")

	f.val[cfParent], f.val[cfTitle] = "abc4", ""
	f.validate()
	require.NotNil(t, f.err)
	require.Equal(t, cfTitle, f.err.field)

	f.kind, f.trigger = "notify", "manual"
	f.val[cfMessage], f.val[cfRecipient], f.val[cfIssue] = "hi", "alice", ""
	f.validate()
	require.NotNil(t, f.err)
	require.Equal(t, cfIssue, f.err.field)
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
	require.JSONEq(t, options, string(def.Options))
	require.Contains(t, string(def.Options), "12345678901234567890123")
	require.Contains(t, string(def.Options), "1.10")
	require.Equal(t, "Line one\nLine two", def.Action.Prompt, "line breaks survive a pass through the editor")
}

func TestCronFormShowsValidationBesideField(t *testing.T) {
	f := newNativeForm(t)
	f.val[cfCron] = "not a schedule"
	f.validate()
	require.NotNil(t, f.err)
	require.Equal(t, cfCron, f.err.field)

	f.val[cfCron] = "0 9 * * *"
	f.step(cfTrigger, 1)
	require.Equal(t, "interval", f.trigger)
	f.val[cfInterval] = "30"
	f.validate()
	require.NotNil(t, f.err)
	require.Equal(t, cfInterval, f.err.field)
	require.Contains(t, f.err.text, "60 seconds")

	f.val[cfInterval] = "15m"
	f.validate()
	require.Nil(t, f.err)
	require.Empty(t, f.general)
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
			require.NoError(t, def.Validate())
		})
	}
}

func TestCronFormBlamesBlankRunIssue(t *testing.T) {
	f := newNativeForm(t)
	f.kind, f.trigger = "prompt", "cron"
	f.val[cfPrompt], f.val[cfPolicy], f.val[cfReport] = "Do it", "existing", ""
	f.validate()
	require.NotNil(t, f.err)
	require.Equal(t, cfReport, f.err.field)
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
	f.val[cfCatchup] = "latest"
	_, def, err := f.build(cronUIDResolver(nil))
	require.NoError(t, err)
	require.Zero(t, def.GraceSeconds)
}
