package tui

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/charmbracelet/x/ansi"
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
		name, status, want, reason string
		edit                       func(*generated.CronJob)
		blocked                    bool
		unreadable                 bool
		prefix                     bool
		runDefinition              *string
	}{
		{name: "success", reason: "last run status is succeeded", status: "succeeded", want: "ok"},
		{name: "cancelled", reason: "last run status is cancelled", status: "cancelled", want: "ok"},
		{name: "failed", reason: "last run status is failed", status: "failed", want: "failing"},
		{name: "unknown", reason: "last run status is unknown", status: "unknown", want: "failing"},
		{name: "running", reason: "", status: "running", want: "running"},
		{name: "overdue", reason: "Timeout exceeded: 60s; started 2026-05-01T11:00:00Z", status: "running", want: "overdue", edit: func(j *generated.CronJob) { j.Definition.TimeoutSeconds = new(int64(60)) }},
		{name: "timeout equality", reason: "", status: "running", want: "running", edit: func(j *generated.CronJob) { j.Definition.TimeoutSeconds = new(int64(3600)) }},
		{name: "blocked", reason: "Stuck", status: "running", want: "blocked", blocked: true},
		{name: "overdue blocked", reason: "Timeout exceeded: 60s; started 2026-05-01T11:00:00Z; Stuck", status: "running", want: "overdue", blocked: true, edit: func(j *generated.CronJob) { j.Definition.TimeoutSeconds = new(int64(60)) }},
		{name: "never", reason: "No scheduled run is past its grace period", want: "never ran"},
		{name: "unavailable", reason: "last run: read failed", want: "unavailable", unreadable: true},
		{name: "paused", reason: "", status: "failed", want: "paused", unreadable: true, edit: func(j *generated.CronJob) { j.Definition.Enabled = false }},
		{name: "missed", reason: "Expected 2026-05-01T07:00:00Z; over 15m0s late (estimate)", want: "missed", edit: func(j *generated.CronJob) {
			j.CreatedAt = now.Add(-25 * time.Hour)
			j.Definition.Trigger = generated.TriggerResponse{Kind: "cron", Cron: new("0 7 * * *")}
		}},
		{name: "once before registration", reason: "Expected 2026-05-01T11:00:00Z; over 15m0s late (estimate)", want: "missed", edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-time.Hour).Format(time.RFC3339))}
		}},
		{name: "once equal registration", reason: "Expected 2026-05-01T11:00:00Z; over 15m0s late (estimate)", want: "missed", edit: func(j *generated.CronJob) {
			j.CreatedAt = now.Add(-time.Hour)
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(j.CreatedAt.Format(time.RFC3339))}
		}},
		{name: "once future", reason: "No scheduled run is past its grace period", want: "never ran", edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(time.Hour).Format(time.RFC3339))}
		}},
		{name: "once observed", reason: "One-time job has run", status: "succeeded", want: "ok", edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-2 * time.Hour).Format(time.RFC3339))}
		}},

		{name: "rescheduled once under older definition", reason: "Expected 2026-05-01T11:30:00Z; over 15m0s late (estimate)", status: "succeeded", want: "missed", runDefinition: new("01ARZ3NDEKTSV4RRFFQ69G5FB1"), edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-30 * time.Minute).Format(time.RFC3339))}
		}},
		{name: "rescheduled once without identity", reason: "Expected 2026-05-01T11:30:00Z; over 15m0s late (estimate)", status: "succeeded", want: "missed", edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-30 * time.Minute).Format(time.RFC3339))}
		}},
		{name: "once manual success under current definition", reason: "One-time job has run under its current definition", status: "succeeded", want: "ok", runDefinition: new("01ARZ3NDEKTSV4RRFFQ69G5FB2"), edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-30 * time.Minute).Format(time.RFC3339))}
		}},
		{name: "once manual cancellation under current definition", reason: "One-time job has run under its current definition", status: "cancelled", want: "ok", runDefinition: new("01ARZ3NDEKTSV4RRFFQ69G5FB2"), edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-30 * time.Minute).Format(time.RFC3339))}
		}},
		{name: "once old definition after instant", reason: "One-time job has run", status: "succeeded", want: "ok", runDefinition: new("01ARZ3NDEKTSV4RRFFQ69G5FB1"), edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(now.Add(-2 * time.Hour).Format(time.RFC3339))}
		}},

		{name: "invalid trigger", prefix: true, reason: "invalid definition: ", want: "unavailable", edit: func(j *generated.CronJob) {
			j.Definition.Trigger = generated.TriggerResponse{Kind: "cron", Cron: new("invalid")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := cronFixture(t).cron.data.jobs[0]
			j.Definition.Trigger = generated.TriggerResponse{Kind: "manual"}
			if tc.edit != nil {
				tc.edit(&j)
			}
			p := cronData{jobs: []generated.CronJob{j}, lastRuns: map[string]generated.CronRun{}, lastErr: map[string]error{}}
			if tc.status != "" {
				p.lastRuns[j.UID] = generated.CronRun{JobUID: new(j.UID), Status: tc.status, CreatedAt: now.Add(-time.Hour), DefinitionEventUID: tc.runDefinition}
			}
			if tc.blocked {
				r := p.lastRuns[j.UID]
				r.Actor, r.IssueUID = "operator", new("issue-example")
				p.lastRuns[j.UID] = r
				p.waits = map[string]cronParent{"issue-example": {open: true, metadata: map[string]any{"work.attention": "stuck"}}}
			}
			if tc.unreadable {
				p.lastErr[j.UID] = errors.New("read failed")
			}
			got := cronHealth(p, j, now)
			require.Equal(t, tc.want, got.state)
			if tc.prefix {
				require.True(t, strings.HasPrefix(got.reason, tc.reason), "%q must start with %q", got.reason, tc.reason)
			} else {
				require.Equal(t, tc.reason, got.reason)
			}
		})
	}
}

func TestCronHealthNativeDelivery(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&j.Definition)
	now := snapshotFixedNow
	p := cronData{parentOpen: map[string]bool{nextParentUID: true}, progress: map[string]cron.RecurringProgress{j.UID: {Version: 1, Anchor: now, Through: now}}, runsErr: errors.New("runs unavailable")}
	r := generated.CronRun{JobUID: new(j.UID), Status: "running", Actor: "operator", IssueUID: new("issue-example")}
	p.jobs = []generated.CronJob{j}
	p.lastRuns = map[string]generated.CronRun{j.UID: r}
	p.waits = map[string]cronParent{"issue-example": {open: true, metadata: map[string]any{"work.attention": "stuck"}}}
	require.Equal(t, "ok", cronHealth(p, j, now).state)
	require.NotEqual(t, "blocked", cronRunHealth(p, r).state)
	p.progress[j.UID] = cron.RecurringProgress{Version: 1, Anchor: now.Add(-2 * time.Hour), Through: now.Add(-2 * time.Hour)}
	require.Equal(t, "missed", cronHealth(p, j, now).state)
	delete(p.progress, j.UID)
	require.Equal(t, "unavailable", cronHealth(p, j, now).state)
	p.parentOpen[nextParentUID] = false
	require.Equal(t, "parent closed", cronHealth(p, j, now).state)
	p.progress[j.UID] = cron.RecurringProgress{Version: 1, Anchor: now, Through: now}
	delete(p.parentOpen, nextParentUID)
	require.Equal(t, "unavailable", cronHealth(p, j, now).state)

	t.Run("observations do not affect delivery", func(t *testing.T) {
		j := cronFixture(t).cron.data.jobs[0]
		nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&j.Definition)
		p := cronData{jobs: []generated.CronJob{j}, parentOpen: map[string]bool{nextParentUID: true}, progress: map[string]cron.RecurringProgress{j.UID: {Version: 1, Anchor: snapshotFixedNow, Through: snapshotFixedNow}}}
		hegel.Test(t, func(ht *hegel.T) {
			p.lastRuns = map[string]generated.CronRun{j.UID: {JobUID: new(j.UID), Status: hegel.Draw(ht, hegel.SampledFrom([]string{"running", "succeeded", "cancelled", "failed", "unknown"})), CreatedAt: hegel.Draw(ht, hegel.Datetimes())}}
			p.lastErr = map[string]error{j.UID: errors.New("unreadable")}
			if got := cronHealth(p, j, snapshotFixedNow); got.state != "ok" {
				ht.Fatalf("observation affected native health: %v", got)
			}
		}, hegel.WithTestCases(2000))
	})
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
		p := cronData{jobs: []generated.CronJob{j}, lastRuns: map[string]generated.CronRun{j.UID: {JobUID: new(j.UID), Status: "failed", CreatedAt: hegel.Draw(ht, hegel.Datetimes())}}, lastErr: map[string]error{}}
		if got := cronHealth(p, j, hegel.Draw(ht, hegel.Datetimes())); got.state != "failing" {
			ht.Fatalf("failed run must be failing: %v", got)
		}
	}, hegel.WithTestCases(2000))
}

func TestCronHealthOnceGrace(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	hegel.Test(t, func(ht *hegel.T) {
		// A fixed instant keeps the oracle about grace, rather than cron arithmetic.
		due := snapshotFixedNow
		j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(due.Format(time.RFC3339))}
		seconds := hegel.Draw(ht, hegel.Integers[int64](0, 86400))
		for _, setting := range []*int64{nil, new(int64(0)), new(seconds)} {
			j.Definition.GraceSeconds = setting
			grace := 15 * time.Minute
			if setting != nil && *setting > 0 {
				grace = time.Duration(*setting) * time.Second
			}
			offset := time.Duration(hegel.Draw(ht, hegel.Integers[int64](-86400, 172800))) * time.Second
			for _, elapsed := range []time.Duration{grace - time.Nanosecond, grace, grace + time.Nanosecond, offset} {
				got := cronHealth(cronData{lastErr: map[string]error{}}, j, due.Add(elapsed))
				if elapsed <= grace {
					if got.state != "never ran" || got.reason != "No scheduled run is past its grace period" {
						ht.Fatalf("health=%v grace=%v elapsed=%v", got, grace, elapsed)
					}
				} else if got.state != "missed" {
					ht.Fatalf("health=%v grace=%v elapsed=%v", got, grace, elapsed)
				}
			}
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
		r := generated.CronRun{JobUID: new(job.UID), Status: "succeeded", CreatedAt: snapshotFixedNow.Add(-time.Hour)}
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
	m = m.cronFilter("job-ok")
	title, _, lines := cronScreen(m)
	require.Len(t, lines, 1)
	require.Contains(t, title, "1 failing", "roll-up is independent of the filter")
	m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, "overdue"}}}
	_, _, lines = cronScreen(m)
	document := cronLinesText(lines)
	require.Contains(t, document, "Health:")
	require.Contains(t, document, "Timeout exceeded: 60s; started 2026-05-01T11:00:00Z")

	{
		m := cronFixture(t)
		m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, m.cron.data.jobs[0].UID}}}
		_, _, lines := cronScreen(m)
		require.NotContains(t, cronLinesText(lines), "State:")
		require.Contains(t, cronLinesText(lines), "Health:")
	}

	assertGolden(t, "cron-jobs-health", cronHealthFixture(t).viewBody())
}

// Both rendering entry points preserve row backgrounds in both themes.
// Selected health text additionally retains at least 4.5:1 contrast.
func TestCronHealthRowBandsAndSelectedContrast(t *testing.T) {
	defer snapshotInit(t)()
	for _, mode := range []colorMode{colorDark, colorLight} {
		for _, cursor := range []int{0, 1} {
			m := cronHealthFixture(t)
			m.width = 120
			m.cron.lists[cronJobs].cursor = cursor
			applyColorMode(mode, mode == colorDark)
			for _, rendered := range []string{renderCron(m), m.viewContent()} {
				lines := strings.Split(rendered, "\n")
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
				if cursor == 1 {
					plain := stripANSI(lines[row])
					x := strings.LastIndex(plain, "failing")
					require.Greater(t, x, 0)
					cell := attentionTerminalCells(lines[row])[ansi.StringWidth(plain[:x])]
					luminance := func(index string) float64 {
						n, err := strconv.ParseUint(index, 10, 8)
						require.NoError(t, err, "health must have an explicit color")
						r, g, b, _ := ansi.IndexedColor(n).RGBA()
						linear := func(v uint32) float64 {
							c := float64(v) / 65535
							if c <= 0.04045 {
								return c / 12.92
							}
							return math.Pow((c+0.055)/1.055, 2.4)
						}
						return 0.2126*linear(r) + 0.7152*linear(g) + 0.0722*linear(b)
					}
					a, b := luminance(cell.foreground), luminance(cell.background)
					ratio := (max(a, b) + 0.05) / (min(a, b) + 0.05)
					t.Logf("health foreground=%s background=%s contrast=%.2f:1", cell.foreground, cell.background, ratio)
					require.GreaterOrEqual(t, ratio, 4.5)
				}
			}
		}
	}
}
