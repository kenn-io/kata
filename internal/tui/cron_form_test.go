package tui

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

const (
	cronTestParent = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	cronTestIssue  = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
)

func cronTestUIDs(string) string { return cronTestIssue }

func newNativeForm(t *testing.T) *cronForm {
	t.Helper()
	f, err := newCronForm(nil, nil)
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
	offset := int64(3600)
	return CronJobRecord{
		UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "Digest", DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB2",
		Definition: cron.JobDefinition{
			Version: 1, Kind: "job", Enabled: true,
			Trigger: cron.Trigger{Kind: "manual"},
			Action:  cron.Action{Kind: "execute", Prompt: "Line one\nLine two"},
			Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Digest", ScheduledOffsetSeconds: &offset},
			Overlap: "forbid", Catchup: "latest", Options: jsontext.Value(options),
		},
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
	f, err := cronFormFor(executeRecord(`{}`), nil, nil)
	require.NoError(t, err)
	_, def, err := f.build(cronTestUIDs)
	require.NoError(t, err)
	require.NotNil(t, def.Issue.ScheduledOffsetSeconds, "per-run keeps its offset")

	f.val[cfPolicy], f.val[cfIssue] = "existing", cronTestIssue
	_, def, err = f.build(cronTestUIDs)
	require.NoError(t, err)
	require.Equal(t, cron.IssuePolicy{Kind: "existing", UID: cronTestIssue}, *def.Issue)
}

func TestCronFormNameUniquenessSkipsUnchangedEdits(t *testing.T) {
	taken := map[string]bool{"digest": true}
	f, err := cronFormFor(executeRecord(`{}`), nil, taken)
	require.NoError(t, err)
	f.val[cfMessage] = "" // an edit elsewhere
	f.val[cfTitle] = "Renamed issue"
	_, _, err = f.build(cronTestUIDs)
	require.NoError(t, err, "the unchanged name is not a duplicate of itself")

	f.val[cfName] = "Digest"
	_, _, err = f.build(cronTestUIDs)
	require.NoError(t, err, "a name equal to the opened one is unchanged")

	f.val[cfName] = "Weekly"
	taken["weekly"] = true
	_, _, err = f.build(cronTestUIDs)
	var fe *cronFieldError
	require.ErrorAs(t, err, &fe)
	require.Equal(t, cfName, fe.field)

	created, err := newCronForm(nil, taken)
	require.NoError(t, err)
	created.val[cfName] = "Digest"
	_, _, err = created.build(cronTestUIDs)
	require.ErrorAs(t, err, &fe)
	require.Equal(t, cfName, fe.field, "a new job may not reuse a name")
}

func TestCronFormLocksNativeParentAndAction(t *testing.T) {
	f, err := cronFormFor(nativeRecord(), nil, nil)
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

func TestCronFormAdapterJobsCannotBecomeNative(t *testing.T) {
	f, err := cronFormFor(executeRecord(`{}`), nil, nil)
	require.NoError(t, err)
	for _, k := range f.kinds() {
		require.NotEqual(t, cronRecurringID, k.id)
	}
}

func TestCronFormNotifyIssueTargetIsExisting(t *testing.T) {
	f, err := newCronForm(nil, nil)
	require.NoError(t, err)
	f.kind, f.trigger = "notify", "manual"
	f.val[cfName], f.val[cfIssue], f.val[cfMessage] = "Ping", cronTestIssue, "hello"
	f.val[cfRecipient] = "alice"
	_, def, err := f.build(cronTestUIDs)
	require.NoError(t, err)
	require.Equal(t, cron.IssuePolicy{Kind: "existing", UID: cronTestIssue}, *def.Issue)

	f.trigger = "issue-deadline"
	f.val[cfReport] = cronTestParent
	_, def, err = f.build(cronTestUIDs)
	require.NoError(t, err)
	require.Equal(t, cron.IssuePolicy{Kind: "existing", UID: cronTestIssue}, *def.Issue)
	require.Equal(t, "issue-deadline", def.Trigger.Kind)
	require.Equal(t, cronTestIssue, def.Trigger.IssueUID)

	f.val[cfReport] = ""
	_, def, err = f.build(cronTestUIDs)
	require.NoError(t, err)
	require.Nil(t, def.Issue, "a date-triggered notify may leave its issue to the trigger")
}

func TestCronFormOptionsAreKeptVerbatim(t *testing.T) {
	const options = `{"future":{"keep":[1,2]},"big":12345678901234567890123,"exact":1.10}`
	f, err := cronFormFor(executeRecord(options), nil, nil)
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

func TestCronFormDirtyTracksEdits(t *testing.T) {
	f := newNativeForm(t)
	f.snapshot = f.val
	require.False(t, f.dirty())
	f.val[cfName] = "Other"
	require.True(t, f.dirty())
}
