package tui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/pkg/client/generated"
)

const (
	nextJobUID    = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	nextParentUID = "01ARZ3NDEKTSV4RRFFQ69G5FB9"
)

// nextRunsDetail opens the fixture job after giving it a trigger and, for a
// native job, the progress its parent issue would hold.
func nextRunsDetail(t *testing.T, now string, edit func(*generated.CronJobDefinitionResponse), progress *cron.RecurringProgress) string {
	t.Helper()
	return nextRunsDetailParent(t, now, edit, progress, true)
}

func nextRunsDetailParent(t *testing.T, now string, edit func(*generated.CronJobDefinitionResponse), progress *cron.RecurringProgress, parentOpen bool) string {
	t.Helper()
	at, err := time.Parse(time.RFC3339, now)
	require.NoError(t, err)
	prior := renderNow
	renderNow = func() time.Time { return at }
	t.Cleanup(func() { renderNow = prior })
	m := cronFixture(t)
	def := m.cron.data.jobs[0].Definition
	edit(&def)
	m.cron.data.jobs[0].Definition = def
	if progress != nil {
		m.cron.data.progress = map[string]cron.RecurringProgress{nextJobUID: *progress}
		m.cron.data.parentOpen = map[string]bool{nextParentUID: parentOpen}
	}
	m = sendKey(m, tea.KeyEnter)
	return ansi.Strip(m.viewBody())
}

func nativeNotify(trigger generated.TriggerResponse) func(*generated.CronJobDefinitionResponse) {
	return func(d *generated.CronJobDefinitionResponse) {
		d.Trigger = trigger
		d.Action = generated.ActionResponse{Kind: "notify", Recipient: new("current-owner-or-author")}
		d.Issue = &generated.IssuePolicyResponse{Kind: "per-run", ParentUID: new(nextParentUID), Title: new("Review")}
		d.Overlap, d.Catchup = "allow", "all"
	}
}

func TestCronJobDetailNativeNextRunsMatchDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, cron, zone, now string
		want                  []string
	}{
		{"fall back", "30 1 * * *", "America/New_York", "2026-11-01T04:00:00Z",
			[]string{"2026-11-01 01:30 EDT", "2026-11-01 01:30 EST", "2026-11-02 01:30 EST"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.now)
			require.NoError(t, err)
			progress := cron.RecurringProgress{Version: 1, Anchor: now, Through: now}
			got := nextRunsDetail(t, tc.now, nativeNotify(generated.TriggerResponse{Kind: "cron", Cron: new(tc.cron), Timezone: new(tc.zone)}), &progress)
			require.Regexp(t, `Next:\s+`+tc.want[0], got)
			for _, line := range tc.want {
				require.Contains(t, got, line)
			}
		})
	}
}

func TestCronJobDetailNativeIntervalSeconds(t *testing.T) {
	for _, tc := range []struct {
		name, now string
		anchor    time.Time
		through   time.Duration
		seconds   int64
		want      []string
	}{
		{"whole minutes", "2026-06-01T05:00:00Z", time.Date(2026, 6, 1, 0, 30, 0, 0, time.UTC), 4 * time.Hour, 7200, []string{"2026-06-01 06:30 UTC", "2026-06-01 08:30 UTC", "2026-06-01 10:30 UTC"}},
		{"seconds", "2026-06-01T00:02:00Z", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), 0, 90, []string{"2026-06-01 00:03:00 UTC", "2026-06-01 00:04:30 UTC", "2026-06-01 00:06:00 UTC"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			progress := cron.RecurringProgress{Version: 1, Anchor: tc.anchor, Through: tc.anchor.Add(tc.through)}
			got := nextRunsDetail(t, tc.now, nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(tc.seconds)}), &progress)
			for _, line := range tc.want {
				require.Contains(t, got, line)
			}
		})
	}
}

func TestCronJobDetailNativeWithoutProgress(t *testing.T) {
	got := nextRunsDetail(t, "2026-06-01T05:00:00Z", nativeNotify(generated.TriggerResponse{Kind: "cron", Cron: new("0 7 * * *")}), nil)
	require.Regexp(t, `Next:\s+unavailable`, got)
}

func TestCronJobDetailAdapterTriggerTimes(t *testing.T) {
	got := nextRunsDetail(t, "2026-03-07T08:00:00Z", func(d *generated.CronJobDefinitionResponse) {
		d.Trigger = generated.TriggerResponse{Kind: "cron", Cron: new("30 2 * * *"), Timezone: new("America/New_York")}
	}, nil)
	require.Regexp(t, `Triggers:\s+2026-03-08 03:30 EDT`, got)
	require.Contains(t, got, "2026-03-10 02:30 EDT")
	require.NotContains(t, got, "Next:")

	got = nextRunsDetail(t, "2026-03-07T08:00:00Z", func(d *generated.CronJobDefinitionResponse) {
		d.Trigger = generated.TriggerResponse{Kind: "once", At: new("2026-04-01T12:00:00Z")}
	}, nil)
	require.Regexp(t, `Triggers:\s+2026-04-01 12:00 UTC`, got)

	got = nextRunsDetail(t, "2026-03-07T08:00:00Z", func(d *generated.CronJobDefinitionResponse) {
		d.Trigger = generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))}
	}, nil)
	require.NotContains(t, got, "Triggers:")
	require.NotContains(t, got, "Next:")
}

func TestCronJobDetailPausedJobHasNoNextRuns(t *testing.T) {
	progress := cron.RecurringProgress{Version: 1, Anchor: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Through: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	native := nativeNotify(generated.TriggerResponse{Kind: "cron", Cron: new("0 7 * * *")})
	got := nextRunsDetail(t, "2026-06-01T05:00:00Z", func(d *generated.CronJobDefinitionResponse) {
		native(d)
		d.Enabled = false
	}, &progress)
	require.Regexp(t, `Next:\s+none while paused`, got)

	got = nextRunsDetail(t, "2026-06-01T05:00:00Z", func(d *generated.CronJobDefinitionResponse) { d.Enabled = false }, nil)
	require.Regexp(t, `Triggers:\s+none while paused`, got)

	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "adapter", true: "native"}[native], func(t *testing.T) {
			got := nextRunsDetail(t, "2026-06-01T05:00:00Z", func(d *generated.CronJobDefinitionResponse) {
				if native {
					nativeNotify(generated.TriggerResponse{Kind: "cron", Cron: new("0 7 * * *")})(d)
				}
				d.Enabled, d.Version = false, 2
			}, nil)
			require.Regexp(t, `(Next|Triggers):\s+unavailable`, got)
		})
	}
}

func TestCronJobDetailNativeClosedParentHasNoNextRuns(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	progress := cron.RecurringProgress{Version: 1, Anchor: now, Through: now}
	got := nextRunsDetailParent(t, "2026-06-01T05:00:00Z", nativeNotify(generated.TriggerResponse{Kind: "cron", Cron: new("0 7 * * *")}), &progress, false)
	require.Regexp(t, `Next:\s+none while the parent is closed`, got)
	require.NotContains(t, got, "2026-06-02 07:00")
}

func TestCronJobDetailAdapterCronUsesOwnGrammar(t *testing.T) {
	got := nextRunsDetail(t, "2026-03-07T08:00:00Z", func(d *generated.CronJobDefinitionResponse) {
		d.Trigger = generated.TriggerResponse{Kind: "cron", Cron: new(`CRON_TZ="Europe/Paris" 0 9 * * *`)}
	}, nil)
	require.Regexp(t, `Triggers:\s+2026-03-08 09:00 CET`, got, "times read in the expression's own zone")
	got = nextRunsDetail(t, "2026-03-07T08:00:00Z", func(d *generated.CronJobDefinitionResponse) {
		d.Trigger = generated.TriggerResponse{Kind: "cron", Cron: new("@every 1h")}
	}, nil)
	require.NotContains(t, got, "Triggers:")
	require.NotContains(t, got, "unavailable")
}

func TestCronScheduleNamesDeadlineLead(t *testing.T) {
	for _, tc := range []struct {
		seconds int64
		want    string
	}{
		{172800, "2 days"},
		{86400, "1 day"},
		{3600, "1 hour"},
		{90000, "25 hours"},
		{5400, "90 minutes"},
		{60, "1 minute"},
		{61, "61 seconds"},
		{1, "1 second"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			require.Equal(t, "issue-deadline, "+tc.want+" before", cronSchedule(generated.TriggerResponse{Kind: "issue-deadline", LeadSeconds: &tc.seconds}))
		})
	}
	require.Equal(t, "issue-deadline", cronSchedule(generated.TriggerResponse{Kind: "issue-deadline"}))
}

type cronProgressTestAPI struct {
	cronReadTestAPI
	progress map[string]cron.RecurringProgress
	parents  []string
}

func (a *cronProgressTestAPI) readCronParent(_ context.Context, _ int64, parent string) (cronParent, error) {
	a.parents = append(a.parents, parent)
	return cronParent{open: true, progress: a.progress}, nil
}

func TestCronLoadReadsProgressOncePerParent(t *testing.T) {
	m := cronFixture(t)
	m.sseCh = nil
	native := nativeNotify(generated.TriggerResponse{Kind: "cron", Cron: new("0 7 * * *")})
	def := m.cron.data.jobs[0].Definition
	native(&def)
	first := m.cron.data.jobs[0]
	first.Definition = def
	second := first
	second.UID, second.Name = "01ARZ3NDEKTSV4RRFFQ69G5FBA", "Second"
	manual := first
	manual.UID, manual.Name = "01ARZ3NDEKTSV4RRFFQ69G5FBB", "Adapter"
	manual.Definition.Action = generated.ActionResponse{Kind: "execute", Prompt: new("go")}
	data := *m.cron.data
	data.jobs = []generated.CronJob{first, second, manual}
	progress := map[string]cron.RecurringProgress{first.UID: {Version: 1, Anchor: snapshotFixedNow, Through: snapshotFixedNow}}
	api := &cronProgressTestAPI{progress: progress}
	api.data = data
	m.api = api
	msg := m.fetchCron()()
	loaded, ok := msg.(cronLoadedMsg)
	require.True(t, ok)
	require.Equal(t, []string{nextParentUID}, api.parents, "one read per parent, none for adapter jobs")
	require.Equal(t, progress, loaded.progress)
	require.Equal(t, map[string]bool{nextParentUID: true}, loaded.parentOpen)

	for _, enabledSibling := range []bool{false, true} {
		t.Run(map[bool]string{false: "paused only", true: "enabled sibling"}[enabledSibling], func(t *testing.T) {
			m := cronFixture(t)
			job := m.cron.data.jobs[0]
			nativeNotify(generated.TriggerResponse{Kind: "cron", Cron: new("0 7 * * *")})(&job.Definition)
			job.Definition.Enabled = false
			api := &cronProgressTestAPI{}
			api.data.jobs = []generated.CronJob{job}
			if enabledSibling {
				job.UID = "01ARZ3NDEKTSV4RRFFQ69G5FBA"
				job.Definition.Enabled = true
				api.data.jobs = append(api.data.jobs, job)
			}
			m.api = api
			m.fetchCron()()
			if enabledSibling {
				require.Equal(t, []string{nextParentUID}, api.parents)
			} else {
				require.Empty(t, api.parents)
			}
		})
	}
}

func TestCronClientReportsClosedParent(t *testing.T) {
	for _, deletedParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "deleted"}[deletedParent], func(t *testing.T) {
			env := testenv.New(t)
			c := NewClient(env.URL, env.HTTP)
			ctx := t.Context()
			project, err := c.EnsureProject(ctx, "cron-progress-example", "worker")
			require.NoError(t, err)
			parent, err := c.CreateIssue(ctx, project.ID, CreateIssueBody{Title: "Recurring parent", Actor: "worker"})
			require.NoError(t, err)
			client, err := c.generatedClient()
			require.NoError(t, err)
			created, err := client.CreateCronJobWithResponse(ctx, &generated.CreateCronJobRequestOptions{
				PathParams: &generated.CreateCronJobPath{ProjectID: project.ID},
				Body: &generated.CreateCronJobBody{
					Actor: new("worker"), Name: "Review service",
					Definition: generated.CronJobDefinition{
						Version: 1, Kind: "job", Enabled: true,
						Trigger: generated.Trigger{Kind: "interval", IntervalSeconds: new(int64(3600))},
						Action:  generated.Action{Kind: "notify", Recipient: new("current-owner-or-author")},
						Issue:   &generated.IssuePolicy{Kind: "per-run", ParentUID: &parent.Issue.UID, Title: new("Review")},
						Overlap: "allow", Catchup: "all",
					},
				},
			})
			require.NoError(t, err)
			require.Equal(t, 201, created.HTTPResponse.StatusCode, string(created.Body))
			jobs, err := c.ListCronJobs(ctx, project.ID)
			require.NoError(t, err)
			require.Len(t, jobs, 1)

			state, err := c.readCronParent(ctx, project.ID, parent.Issue.UID)
			require.NoError(t, err)
			require.True(t, state.open)
			require.Contains(t, state.progress, jobs[0].UID)
			got := state.progress[jobs[0].UID]
			require.Equal(t, 1, got.Version)
			require.False(t, got.Anchor.IsZero())
			require.NoError(t, got.Validate())

			if deletedParent {
				deleted, err := client.DeleteIssueWithResponse(ctx, &generated.DeleteIssueRequestOptions{
					PathParams: &generated.DeleteIssuePath{ProjectID: project.ID, Ref: parent.Issue.UID},
					Body:       &generated.DeleteIssueBody{Actor: "worker"},
					Header:     &generated.DeleteIssueHeaders{XKataConfirm: new("DELETE " + project.Name + "#" + parent.Issue.ShortID)},
				})
				require.NoError(t, err)
				require.Equal(t, 200, deleted.HTTPResponse.StatusCode, string(deleted.Body))
			} else {
				_, err = c.Close(ctx, project.ID, parent.Issue.UID, "worker")
				require.NoError(t, err)
			}

			state, err = c.readCronParent(ctx, project.ID, parent.Issue.UID)
			require.NoError(t, err)
			require.False(t, state.open)
		})
	}
}

func TestCronProgressSkipsOneCorruptEntry(t *testing.T) {
	good := map[string]any{"version": 1, "anchor": "2026-06-01T00:00:00Z", "through": "2026-06-01T00:00:00Z"}
	got := cronProgressFromMetadata(map[string]any{
		"cron.progress." + nextJobUID: good,
		"cron.progress.BROKEN":        map[string]any{"version": "one", "anchor": 12},
		"cron.progress.UNKNOWN":       map[string]any{"version": 1, "anchor": "2026-06-01T00:00:00Z", "through": "2026-06-01T00:00:00Z", "future": true},
		"cron.progress.NEWER":         map[string]any{"version": 2, "anchor": "2026-06-01T00:00:00Z", "through": "2026-06-01T00:00:00Z"},
		"cron.occurrence":             "ignored",
	})
	require.Len(t, got, 1, "a corrupt checkpoint hides only its own job")
	require.Contains(t, got, nextJobUID)
	require.Equal(t, 1, got[nextJobUID].Version)
}

func TestCronProjectArchiveAndRestoreTogglesNextRuns(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	ctx := t.Context()
	project, err := c.EnsureProject(ctx, "cron-archive-example", "worker")
	require.NoError(t, err)
	parent, err := c.CreateIssue(ctx, project.ID, CreateIssueBody{Title: "Recurring parent", Actor: "worker"})
	require.NoError(t, err)
	client, err := c.generatedClient()
	require.NoError(t, err)
	created, err := client.CreateCronJobWithResponse(ctx, &generated.CreateCronJobRequestOptions{
		PathParams: &generated.CreateCronJobPath{ProjectID: project.ID},
		Body: &generated.CreateCronJobBody{
			Actor: new("worker"), Name: "Review service",
			Definition: generated.CronJobDefinition{
				Version: 1, Kind: "job", Enabled: true,
				Trigger: generated.Trigger{Kind: "interval", IntervalSeconds: new(int64(3600))},
				Action:  generated.Action{Kind: "notify", Recipient: new("current-owner-or-author")},
				Issue:   &generated.IssuePolicy{Kind: "per-run", ParentUID: &parent.Issue.UID, Title: new("Review")},
				Overlap: "allow", Catchup: "all",
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 201, created.HTTPResponse.StatusCode, string(created.Body))

	m := newTestModel()
	m.width, m.height = 80, 24
	m.api, m.sseCh = c, nil
	m.scope.projectID, m.scope.projectName = project.ID, project.Name
	m.view, m.cron.tab = viewCron, cronJobs
	m, _ = updateModel(m, m.fetchCron()())
	m = sendKey(m, tea.KeyEnter)
	require.Regexp(t, `Next:\s+\d{4}-\d{2}-\d{2}`, ansi.Strip(m.viewBody()))

	// Another client force-archives the project while its job detail stays open.
	removed, err := client.RemoveProjectWithResponse(ctx, &generated.RemoveProjectRequestOptions{
		PathParams: &generated.RemoveProjectPath{ProjectID: project.ID},
		Query:      &generated.RemoveProjectQuery{Actor: "worker", Force: new(true)},
	})
	require.NoError(t, err)
	require.Equal(t, 200, removed.HTTPResponse.StatusCode, string(removed.Body))
	require.NotNil(t, removed.JSON200)
	m, cmd := updateModel(m, eventReceivedMsg{eventType: removed.JSON200.Event.Type, projectID: project.ID})
	refreshed := false
	for _, msg := range runCmdTree(cmd) {
		if loaded, ok := msg.(cronLoadedMsg); ok {
			require.ErrorContains(t, loaded.jobsErr, "project_not_found")
			m, _ = updateModel(m, loaded)
			refreshed = true
		}
	}
	require.True(t, refreshed, "project removal must refresh the loaded jobs")
	got := ansi.Strip(m.viewBody())
	require.Contains(t, got, "Review service", "keep the loaded job detail")
	require.Regexp(t, `Next:\s+unavailable`, got)
	require.NotRegexp(t, `Next:\s+\d{4}-\d{2}-\d{2}`, got)

	restored, err := client.RestoreProjectWithResponse(ctx, &generated.RestoreProjectRequestOptions{
		PathParams: &generated.RestoreProjectPath{ProjectID: project.ID},
		Query:      &generated.RestoreProjectQuery{Actor: "worker"},
	})
	require.NoError(t, err)
	require.Equal(t, 200, restored.HTTPResponse.StatusCode, string(restored.Body))
	m, cmd = updateModel(m, eventReceivedMsg{eventType: "project.restored", projectID: project.ID})
	refreshed = false
	for _, msg := range runCmdTree(cmd) {
		if loaded, ok := msg.(cronLoadedMsg); ok {
			require.NoError(t, loaded.jobsErr)
			m, _ = updateModel(m, loaded)
			refreshed = true
		}
	}
	require.True(t, refreshed, "project restoration must refresh the loaded jobs")
	require.Regexp(t, `Next:\s+\d{4}-\d{2}-\d{2}`, ansi.Strip(m.viewBody()))
}

// A change to a native job's parent issue made elsewhere must reload the
// parent's status and progress, even while another tab is showing.
func TestCronReloadsWhenNativeParentChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event eventReceivedMsg
		away  bool
		want  int
	}{
		{"closed", eventReceivedMsg{eventType: "issue.closed", projectID: 7, issueUID: nextParentUID}, false, 1},
		{"closed while on another tab", eventReceivedMsg{eventType: "issue.closed", projectID: 7, issueUID: nextParentUID}, true, 1},
		{"unrelated issue", eventReceivedMsg{eventType: "issue.closed", projectID: 7, issueUID: "01ARZ3NDEKTSV4RRFFQ69G5FC0"}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cronFixture(t)
			m.sseCh = nil
			def := m.cron.data.jobs[0].Definition
			nativeNotify(generated.TriggerResponse{Kind: "cron", Cron: new("0 7 * * *")})(&def)
			m.cron.data.jobs[0].Definition = def
			m.cron.data.parentOpen = map[string]bool{nextParentUID: true}
			api := &cronProgressTestAPI{progress: map[string]cron.RecurringProgress{}}
			api.data = *m.cron.data
			m.api = api
			if tc.away {
				m = sendRune(m, '1')
			}
			m, cmd := updateModel(m, tc.event)
			if tc.away {
				runCmdTree(cmd)
				require.Empty(t, api.parents, "no read while away")
				m, cmd = updateModel(m, keyRune('2'))
			}
			runCmdTree(cmd)
			require.Len(t, api.parents, tc.want)
		})
	}
}
