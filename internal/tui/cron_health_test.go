package tui

import (
	"errors"
	"fmt"
	"slices"
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
			if tc.name == "overdue" {
				require.Equal(t, "running past timeout_seconds 60 (started 2026-05-01T11:00:00Z)", got.reason)
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

// Daily UTC schedules admit an independent calendar oracle, including the
// supported year boundary. Datetimes spans years 1..9999; no DST is involved.
func TestCronHealthDailyScheduleAndGrace(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	hegel.Test(t, func(ht *hegel.T) {
		minute := hegel.Draw(ht, hegel.Integers(0, 59))
		hour := hegel.Draw(ht, hegel.Integers(0, 23))
		j.Definition.Trigger = generated.TriggerResponse{Kind: "cron", Cron: new(fmt.Sprintf("%d %d * * *", minute, hour))}
		base := hegel.Draw(ht, hegel.Datetimes())
		j.CreatedAt = base
		j.Definition.GraceSeconds = new(hegel.Draw(ht, hegel.Integers[int64](0, 86400)))
		now := base.Add(time.Duration(hegel.Draw(ht, hegel.Integers[int64](0, 7*86400))) * time.Second)
		due := time.Date(base.Year(), base.Month(), base.Day(), hour, minute, 0, 0, time.UTC)
		if !due.After(base) {
			due = due.AddDate(0, 0, 1)
		}
		grace := 15 * time.Minute
		if *j.Definition.GraceSeconds > 0 {
			grace = time.Duration(*j.Definition.GraceSeconds) * time.Second
		}
		want := due.Year() <= 9999 && due.Add(grace).Before(now)
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

	assertGolden(t, "cron-jobs-health", cronHealthFixture(t).viewBody())
}

func cronLinesText(lines []cronLine) string {
	text := ""
	for _, line := range lines {
		text += line.text + "\n"
	}
	return text
}

func TestCronHealthCellsKeepRowBand(t *testing.T) {
	defer snapshotInit(t)()
	for _, mode := range []colorMode{colorDark, colorLight} {
		for _, cursor := range []int{0, 1} {
			m := cronHealthFixture(t)
			m.width = 120
			m.cron.lists[cronJobs].cursor = cursor
			applyColorMode(mode, mode == colorDark)
			lines := strings.Split(renderCron(m), "\n")
			row := slices.IndexFunc(lines, func(line string) bool { return strings.Contains(stripANSI(line), "job-failing") })
			require.NotEqual(t, -1, row)
			style := altRowStyle
			if cursor == 1 {
				style = cursorRowStyle
			}
			expected := attentionTerminalCells(style.Render("x"))[0]
			cells := attentionTerminalCells(lines[row])
			require.Len(t, cells, m.width)
			for x, cell := range cells {
				require.Equal(t, expected.background, cell.background, "mode %v cursor %d column %d", mode, cursor, x)
			}
		}
	}
}

func TestCronHealthOverdueWinsAndNativeIgnoresWait(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	j.Definition.Trigger = generated.TriggerResponse{Kind: "manual"}
	j.Definition.TimeoutSeconds = new(int64(60))
	r := generated.CronRun{Status: "running", Actor: "operator", IssueUID: new("issue-example"), JobUID: new(j.UID), CreatedAt: snapshotFixedNow.Add(-time.Hour)}
	p := cronData{jobs: []generated.CronJob{j}, lastRuns: map[string]generated.CronRun{j.UID: r}, lastErr: map[string]error{}, waits: map[string]cronParent{"issue-example": {open: true, metadata: map[string]any{"work.attention": "stuck"}}}}
	got := cronHealth(p, j, snapshotFixedNow)
	require.Equal(t, "overdue", got.state)
	require.Contains(t, got.reason, "Stuck")
	j.Definition.TimeoutSeconds = nil
	require.Equal(t, "blocked", cronHealth(p, j, snapshotFixedNow).state)
	nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&j.Definition)
	p.jobs = []generated.CronJob{j}
	p.parentOpen = map[string]bool{nextParentUID: true}
	p.progress = map[string]cron.RecurringProgress{j.UID: {Version: 1, Anchor: snapshotFixedNow, Through: snapshotFixedNow}}
	require.Equal(t, "ok", cronHealth(p, j, snapshotFixedNow).state)
	require.NotEqual(t, "blocked", cronRunHealth(p, r).state)
}
