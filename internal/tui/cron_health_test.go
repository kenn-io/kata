package tui

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

// Contract: health describes fresh observations for adapters, and delivery
// progress alone for native jobs. Timeout/grace comparisons are strict.
func TestCronHealthStates(t *testing.T) {
	now := snapshotFixedNow
	for _, tc := range []struct {
		name, status, want string
		edit               func(*generated.CronJob)
		unreadable         bool
		runDefinition      *string
	}{
		{name: "success", status: "succeeded", want: "ok"},
		{name: "cancelled", status: "cancelled", want: "ok"},
		{name: "failed", status: "failed", want: "failing"},
		{name: "unknown", status: "unknown", want: "failing"},
		{name: "running", status: "running", want: "running"},
		{name: "overdue", status: "running", want: "overdue", edit: func(j *generated.CronJob) { j.Definition.TimeoutSeconds = new(int64(60)) }},
		{name: "timeout equality", status: "running", want: "running", edit: func(j *generated.CronJob) { j.Definition.TimeoutSeconds = new(int64(3600)) }},
		{name: "never", want: "never ran"},
		{name: "unavailable", want: "unavailable", unreadable: true},
		{name: "paused", status: "failed", want: "paused", unreadable: true, edit: func(j *generated.CronJob) { j.Definition.Enabled = false }},
		{name: "missed", want: "missed", edit: func(j *generated.CronJob) {
			j.CreatedAt = now.Add(-25 * time.Hour)
			j.Definition.Trigger = generated.TriggerResponse{Kind: "cron", Cron: new("0 7 * * *")}
		}},
		{name: "once before registration", want: "missed", edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-time.Hour).Format(time.RFC3339))}
		}},
		{name: "once equal registration", want: "missed", edit: func(j *generated.CronJob) {
			j.CreatedAt = now.Add(-time.Hour)
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(j.CreatedAt.Format(time.RFC3339))}
		}},
		{name: "once future", want: "never ran", edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(time.Hour).Format(time.RFC3339))}
		}},
		{name: "once observed", status: "succeeded", want: "ok", edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-2 * time.Hour).Format(time.RFC3339))}
		}},

		{name: "rescheduled once under older definition", status: "succeeded", want: "missed", runDefinition: new("01ARZ3NDEKTSV4RRFFQ69G5FB1"), edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-30 * time.Minute).Format(time.RFC3339))}
		}},
		{name: "rescheduled once without identity", status: "succeeded", want: "missed", edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-30 * time.Minute).Format(time.RFC3339))}
		}},
		{name: "once manual success under current definition", status: "succeeded", want: "ok", runDefinition: new("01ARZ3NDEKTSV4RRFFQ69G5FB2"), edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-30 * time.Minute).Format(time.RFC3339))}
		}},
		{name: "once manual cancellation under current definition", status: "cancelled", want: "ok", runDefinition: new("01ARZ3NDEKTSV4RRFFQ69G5FB2"), edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-30 * time.Minute).Format(time.RFC3339))}
		}},
		{name: "once old definition after instant", status: "succeeded", want: "ok", runDefinition: new("01ARZ3NDEKTSV4RRFFQ69G5FB1"), edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-2 * time.Hour).Format(time.RFC3339))}
		}},
		{name: "grace equality", want: "never ran", edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-15 * time.Minute).Format(time.RFC3339))}
		}},
		{name: "invalid trigger", want: "unavailable", edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "cron", Cron: new("invalid")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := cronFixture(t).cron.data.jobs[0]
			j.Definition.Trigger = generated.TriggerResponse{Kind: "manual"}
			if tc.edit != nil {
				tc.edit(&j)
			}
			p := cronData{lastRuns: map[string]generated.CronRun{}, lastErr: map[string]error{}}
			if tc.status != "" {
				p.lastRuns[j.UID] = generated.CronRun{JobUID: new(j.UID), Status: tc.status, CreatedAt: now.Add(-time.Hour), DefinitionEventUID: tc.runDefinition}
			}
			if tc.unreadable {
				p.lastErr[j.UID] = errors.New("read failed")
			}
			got := cronHealth(p, j, now)
			require.Equal(t, tc.want, got.state)
			if got.state != "paused" && got.state != "running" {
				require.NotEmpty(t, got.reason)
			}
		})
	}
}

func TestCronHealthNativeDelivery(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&j.Definition)
	now := snapshotFixedNow
	p := cronData{parentOpen: map[string]bool{nextParentUID: true}, progress: map[string]cron.RecurringProgress{j.UID: {Version: 1, Anchor: now, Through: now}}, runsErr: errors.New("runs unavailable")}
	require.Equal(t, "ok", cronHealth(p, j, now).state)
	p.progress[j.UID] = cron.RecurringProgress{Version: 1, Anchor: now.Add(-2 * time.Hour), Through: now.Add(-2 * time.Hour)}
	require.Equal(t, "missed", cronHealth(p, j, now).state)
	delete(p.progress, j.UID)
	require.Equal(t, "unavailable", cronHealth(p, j, now).state)
	p.parentOpen[nextParentUID] = false
	require.Equal(t, "parent closed", cronHealth(p, j, now).state)
	delete(p.parentOpen, nextParentUID)
	require.Equal(t, "unavailable", cronHealth(p, j, now).state)
	p.parentOpen[nextParentUID] = true
	p.progress[j.UID] = cron.RecurringProgress{Version: 9, Anchor: now, Through: now}
	require.Equal(t, "unavailable", cronHealth(p, j, now).state, "corrupt progress cannot establish delivery health")
}

func TestCronHealthDisabledAlwaysPaused(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		j := generated.CronJob{Definition: generated.CronJobDefinitionResponse{Enabled: false, Trigger: generated.TriggerResponse{Kind: hegel.Draw(ht, hegel.SampledFrom([]string{"manual", "cron", "interval", "once", "issue-deadline"}))}}}
		got := cronHealth(cronData{jobsErr: errors.New("offline")}, j, hegel.Draw(ht, hegel.Datetimes()))
		if got.state != "paused" {
			ht.Fatalf("disabled health: %s", got.state)
		}
	}, hegel.WithTestCases(2000))
}

func TestCronHealthAdapterIntervalNeverMissed(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	hegel.Test(t, func(ht *hegel.T) {
		j.Definition.Trigger = generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(hegel.Draw(ht, hegel.Integers[int64](1, 31536000)))}
		j.CreatedAt = hegel.Draw(ht, hegel.Datetimes())
		if got := cronHealth(cronData{lastRuns: map[string]generated.CronRun{}, lastErr: map[string]error{}}, j, hegel.Draw(ht, hegel.Datetimes())); got.state == "missed" {
			ht.Fatal("adapter interval has no anchor")
		}
	}, hegel.WithTestCases(2000))
}

func TestCronHealthFailedNeverOK(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	hegel.Test(t, func(ht *hegel.T) {
		p := cronData{lastRuns: map[string]generated.CronRun{j.UID: {Status: "failed", CreatedAt: hegel.Draw(ht, hegel.Datetimes())}}, lastErr: map[string]error{}}
		if got := cronHealth(p, j, hegel.Draw(ht, hegel.Datetimes())); got.state != "failing" {
			ht.Fatalf("failed run must be failing: %v", got)
		}
	}, hegel.WithTestCases(2000))
}

func TestCronHealthNativeIgnoresObservations(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&j.Definition)
	p := cronData{parentOpen: map[string]bool{nextParentUID: true}, progress: map[string]cron.RecurringProgress{j.UID: {Version: 1, Anchor: snapshotFixedNow, Through: snapshotFixedNow}}}
	hegel.Test(t, func(ht *hegel.T) {
		p.lastRuns = map[string]generated.CronRun{j.UID: {Status: hegel.Draw(ht, hegel.Text()), CreatedAt: hegel.Draw(ht, hegel.Datetimes())}}
		p.lastErr = map[string]error{j.UID: errors.New("unreadable")}
		if got := cronHealth(p, j, snapshotFixedNow); got.state != "ok" {
			ht.Fatalf("observation affected native health: %v", got)
		}
	}, hegel.WithTestCases(2000))
}

func TestCronHealthMissedAgreesWithSchedule(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	hegel.Test(t, func(ht *hegel.T) {
		j.Definition.Trigger = generated.TriggerResponse{Kind: "cron", Cron: new(fmt.Sprintf("%d %d * * *", hegel.Draw(ht, hegel.Integers(0, 59)), hegel.Draw(ht, hegel.Integers(0, 23))))}
		base := hegel.Draw(ht, hegel.Datetimes())
		j.CreatedAt = base
		j.Definition.GraceSeconds = new(hegel.Draw(ht, hegel.Integers[int64](0, 86400)))
		now := base.Add(time.Duration(hegel.Draw(ht, hegel.Integers[int64](0, 7*86400))) * time.Second)
		var def cron.JobDefinition
		raw, err := json.Marshal(j.Definition)
		if err != nil {
			ht.Fatal(err)
		}
		if err := json.Unmarshal(raw, &def); err != nil {
			ht.Fatal(err)
		}
		due, err := cron.RecurringOccurrences(def, cron.RecurringProgress{}, base, 1)
		if err != nil {
			ht.Fatal(err)
		}
		grace := 15 * time.Minute
		if def.GraceSeconds > 0 {
			grace = time.Duration(def.GraceSeconds) * time.Second
		}
		want := len(due) > 0 && due[0].Before(now.Add(-grace))
		got := cronHealth(cronData{lastErr: map[string]error{}}, j, now)
		if (got.state == "missed") != want {
			ht.Fatalf("missed=%v want=%v base=%s due=%v now=%s", got, want, base, due, now)
		}
	}, hegel.WithTestCases(2000))
}

func cronHealthFixture(t *testing.T) Model {
	t.Helper()
	m := cronFixture(t)
	m.width, m.height = 160, 26
	j := m.cron.data.jobs[0]
	m.cron.data.jobs = nil
	p := m.cron.data
	p.lastRuns = map[string]generated.CronRun{}
	p.lastErr = map[string]error{}
	p.parentOpen = map[string]bool{nextParentUID: false}
	for _, state := range []string{"ok", "failing", "overdue", "running", "missed", "never ran", "paused", "unavailable", "parent closed"} {
		job := j
		job.UID = state
		job.Name = "job-" + strings.ReplaceAll(state, " ", "-")
		job.Definition.Trigger = generated.TriggerResponse{Kind: "manual"}
		r := generated.CronRun{Status: "succeeded", CreatedAt: snapshotFixedNow.Add(-time.Hour)}
		switch state {
		case "failing":
			r.Status = "failed"
		case "overdue":
			r.Status = "running"
			job.Definition.TimeoutSeconds = new(int64(60))
		case "running":
			r.Status = "running"
		case "missed":
			job.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(snapshotFixedNow.Add(-time.Hour).Format(time.RFC3339))}
		case "paused":
			job.Definition.Enabled = false
		case "unavailable":
			p.lastErr[job.UID] = errors.New("offline")
		case "parent closed":
			nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&job.Definition)
		}
		if state == "ok" || state == "failing" || state == "overdue" || state == "running" {
			p.lastRuns[job.UID] = r
		}
		p.jobs = append(p.jobs, job)
	}
	return m
}

func TestCronHealthColumnRollupAndReason(t *testing.T) {
	defer snapshotInit(t)()
	m := cronHealthFixture(t)
	title, header, lines := cronScreen(m)
	require.Contains(t, header, "Health")
	require.Contains(t, header, "Last run")
	for _, state := range []string{"paused", "unavailable", "parent closed", "overdue", "running", "failing", "missed", "never ran"} {
		require.Contains(t, title, "1 "+state)
	}
	for _, line := range lines {
		require.Contains(t, line.text, cronHealth(*m.cron.data, *m.cron.data.job(line.target.uid), snapshotFixedNow).state)
	}
	m = m.cronFilter("job-ok")
	title, _, lines = cronScreen(m)
	require.Len(t, lines, 1)
	require.Contains(t, title, "1 failing", "roll-up is independent of the filter")
	m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, "overdue"}}}
	_, _, lines = cronScreen(m)
	document := cronLinesText(lines)
	require.Contains(t, document, "Health:")
	require.Contains(t, document, "running past timeout_seconds 60")

	{
		m := cronFixture(t)
		m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, m.cron.data.jobs[0].UID}}}
		_, _, lines := cronScreen(m)
		require.NotContains(t, cronLinesText(lines), "State:")
		require.Contains(t, cronLinesText(lines), "Health:")
	}

	{
		defer snapshotInit(t)()
		assertGolden(t, "cron-jobs-health", cronHealthFixture(t).viewBody())
	}
}

func cronLinesText(lines []cronLine) string {
	text := ""
	for _, line := range lines {
		text += line.text + "\n"
	}
	return text
}
