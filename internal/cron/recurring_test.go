package cron

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

const recurringJSON = `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":7200},"action":{"kind":"notify","recipient":"current-owner-or-author"},"issue":{"kind":"per-run","parent_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","title":"Review service"},"overlap":"allow","catchup":"all"}`

// The native shape must be accepted without accepting adapter/execution fields.
func TestRecurringDefinition(t *testing.T) {
	job, err := ParseJob([]byte(recurringJSON))
	require.NoError(t, err)
	require.Equal(t, "per-run", job.Issue.Kind)
	for name, change := range map[string]func(*JobDefinition){
		"missing parent":     func(j *JobDefinition) { j.Issue.ParentUID = "" },
		"invalid parent":     func(j *JobDefinition) { j.Issue.ParentUID = "invalid" },
		"existing UID":       func(j *JobDefinition) { j.Issue.UID = j.Issue.ParentUID },
		"overlap forbids":    func(j *JobDefinition) { j.Overlap = "forbid" },
		"manual":             func(j *JobDefinition) { j.Trigger = Trigger{Kind: "manual"} },
		"offset":             func(j *JobDefinition) { var n int64; j.Issue.ScheduledOffsetSeconds = &n },
		"timeout":            func(j *JobDefinition) { j.TimeoutSeconds = 1 },
		"secret":             func(j *JobDefinition) { j.SecretRefs = map[string]string{"key": "reference"} },
		"options":            func(j *JobDefinition) { j.Options = []byte(`{"adapter":true}`) },
		"skip without grace": func(j *JobDefinition) { j.Catchup = "skip" },
		"every descriptor":   func(j *JobDefinition) { j.Trigger = Trigger{Kind: "cron", Cron: "@every 2h"} },
		"tz directive every descriptor": func(j *JobDefinition) {
			j.Trigger = Trigger{Kind: "cron", Cron: "TZ=UTC @every 2h"}
		},
		"cron tz directive every descriptor": func(j *JobDefinition) {
			j.Trigger = Trigger{Kind: "cron", Cron: "CRON_TZ=UTC @every 2h"}
		},
		"conflicting timezone": func(j *JobDefinition) {
			j.Trigger = Trigger{Kind: "cron", Cron: "CRON_TZ=America/New_York 0 9 * * *", Timezone: "UTC"}
		},
		"title length":        func(j *JobDefinition) { j.Issue.Title = strings.Repeat("x", 1024) },
		"sub-minute interval": func(j *JobDefinition) { j.Trigger.IntervalSeconds = 59 },
	} {
		t.Run(name, func(t *testing.T) {
			var j JobDefinition
			require.NoError(t, json.Unmarshal([]byte(recurringJSON), &j))
			change(&j)
			require.ErrorIs(t, j.Validate(), ErrInvalid)
		})
	}
	job.Issue.Title = strings.Repeat("é", 991)
	require.NoError(t, job.Validate(), "991 characters plus the 33-character timestamp suffix fit in 1024")
	job.Issue.Title = strings.Repeat("é", 992)
	require.ErrorIs(t, job.Validate(), ErrInvalid)
	job.Issue.Title = "Review service"
	job.Trigger.IntervalSeconds = 60
	require.NoError(t, job.Validate(), "one minute is the shortest native interval")
	job.Enabled = false
	require.NoError(t, job.Validate())
	job.Catchup = "skip"
	job.GraceSeconds = 60
	require.NoError(t, job.Validate())
	job.Action.Recipient = "operator/worker"
	require.NoError(t, job.Validate(), "native notifications have a default message for fixed recipients too")
	legacy, err := ParseJob([]byte(executeJSON))
	require.NoError(t, err)
	legacy.Issue.ParentUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	require.ErrorIs(t, legacy.Validate(), ErrInvalid, "parent binding is native only")
}

func at(t testing.TB, s string) time.Time {
	t.Helper()
	v, e := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, e)
	return v
}
func nativeJob(t testing.TB) JobDefinition {
	t.Helper()
	j, e := ParseJob([]byte(recurringJSON))
	require.NoError(t, e)
	return j
}

const definitionUID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"

// Calendar occurrences use scheduled instants, with bounded independent catch-up.
func TestCalendarAndCatchup(t *testing.T) {
	anchor := at(t, "2026-06-01T00:00:00Z")
	progress := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}
	j := nativeJob(t)
	j.Trigger = Trigger{Kind: "cron", Cron: "0 0 1 * *"}
	now := at(t, "2026-09-15T00:00:00Z")
	all, e := PlanRecurring(j, definitionUID, progress, now)
	require.NoError(t, e)
	require.Equal(t, []time.Time{at(t, "2026-07-01T00:00:00Z"), at(t, "2026-08-01T00:00:00Z"), at(t, "2026-09-01T00:00:00Z")}, all.Due)
	j.Catchup = "latest"
	latest, e := PlanRecurring(j, definitionUID, progress, now)
	require.NoError(t, e)
	require.Equal(t, []time.Time{at(t, "2026-09-01T00:00:00Z")}, latest.Due)
	j.Trigger = Trigger{Kind: "cron", Cron: "* * * * *"}
	now = anchor.Add(2005 * time.Minute)
	scan, e := PlanRecurring(j, definitionUID, progress, now)
	require.NoError(t, e)
	require.Empty(t, scan.Due)
	require.True(t, scan.More)
	require.NotNil(t, scan.Progress.Scan)
	backward, e := PlanRecurring(j, definitionUID, scan.Progress, anchor.Add(time.Minute))
	require.NoError(t, e)
	require.Empty(t, backward.Due)
	for scan.More {
		scan, e = PlanRecurring(j, definitionUID, scan.Progress, now)
		require.NoError(t, e)
	}
	require.Equal(t, []time.Time{now}, scan.Due)
	edited, e := PlanRecurring(j, "01ARZ3NDEKTSV4RRFFQ69G5FAX", backward.Progress, anchor.Add(10*time.Minute))
	require.NoError(t, e)
	require.Equal(t, anchor, edited.Progress.Anchor)
	require.Equal(t, []time.Time{anchor.Add(10 * time.Minute)}, edited.Due)
	j = nativeJob(t)
	j.Catchup = "skip"
	j.GraceSeconds = 60
	skipped, e := PlanRecurring(j, definitionUID, progress, anchor.Add(4*time.Hour+time.Second))
	require.NoError(t, e)
	require.Equal(t, []time.Time{anchor.Add(4 * time.Hour)}, skipped.Due)
	none, e := PlanRecurring(j, definitionUID, progress, anchor.Add(-time.Hour))
	require.NoError(t, e)
	require.Equal(t, progress, none.Progress)
	j.Catchup = "all"
	batch, e := PlanRecurring(j, definitionUID, progress, anchor.Add(300*time.Hour))
	require.NoError(t, e)
	require.Len(t, batch.Due, 50)
	require.True(t, batch.More)
	for _, tc := range []struct {
		cron, start, end string
		want             []string
	}{
		{"30 2 * * *", "2026-03-07T08:00:00Z", "2026-03-09T07:00:00Z", []string{"2026-03-09T06:30:00Z"}},
		{"30 1 * * *", "2026-11-01T04:00:00Z", "2026-11-01T07:00:00Z", []string{"2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"}},
	} {
		j.Trigger = Trigger{Kind: "cron", Cron: tc.cron, Timezone: "America/New_York"}
		a := at(t, tc.start)
		b, e := PlanRecurring(j, definitionUID, RecurringProgress{Version: 1, Anchor: a, Through: a}, at(t, tc.end))
		require.NoError(t, e)
		var want []time.Time
		for _, s := range tc.want {
			want = append(want, at(t, s))
		}
		require.Equal(t, want, b.Due)
	}
}

// Across interval size and backlog, all-catchup must deliver the exact prefix once.
func TestRecurringIntervalPrefixProperty(t *testing.T) {
	base := nativeJob(t)
	rapid.Check(t, func(t *rapid.T) {
		seconds := rapid.Int64Range(60, maxDurationSeconds).Draw(t, "seconds")
		count := rapid.Uint64().Draw(t, "count")
		// Materialize a bounded timeline; generation still spans the duration domain.
		n := int(count%200) + 1
		if int64(n) > maxDurationSeconds/seconds {
			n = int(maxDurationSeconds / seconds)
		}
		a := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		j := base
		j.Trigger.IntervalSeconds = seconds
		p := RecurringProgress{Version: 1, Anchor: a, Through: a}
		now := a.Add(time.Duration(seconds*int64(n)) * time.Second)
		var got []time.Time
		for {
			b, e := PlanRecurring(j, definitionUID, p, now)
			require.NoError(t, e)
			require.LessOrEqual(t, len(b.Due), 50)
			got = append(got, b.Due...)
			p = b.Progress
			if !b.More {
				break
			}
		}
		var want []time.Time
		for i := 1; i <= n; i++ {
			want = append(want, a.Add(time.Duration(seconds*int64(i))*time.Second))
		}
		require.Equal(t, want, got)
	})
}

// Skip converges immediately to its grace window even after a long calendar outage.
func TestRecurringCalendarSkipFastForward(t *testing.T) {
	j := nativeJob(t)
	j.Trigger = Trigger{Kind: "cron", Cron: "* * * * *"}
	j.Catchup = "skip"
	j.GraceSeconds = 3600
	anchor := at(t, "2026-01-01T00:00:00Z")
	now := anchor.Add(30 * 24 * time.Hour)
	batch, e := PlanRecurring(j, definitionUID, RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}, now)
	require.NoError(t, e)
	require.Len(t, batch.Due, 50)
	require.Equal(t, now.Add(-time.Hour), batch.Due[0], "an occurrence exactly on grace is retained")
	require.True(t, batch.More)
	next, e := PlanRecurring(j, definitionUID, batch.Progress, now)
	require.NoError(t, e)
	require.Len(t, next.Due, 11)
	require.Equal(t, now, next.Due[10])
	require.False(t, next.More)
	replay, e := PlanRecurring(j, definitionUID, next.Progress, now)
	require.NoError(t, e)
	require.Empty(t, replay.Due)
	require.Equal(t, next.Progress, replay.Progress)
}

// Skip leaves progress untouched while no occurrence has passed, however often it sweeps.
func TestRecurringSkipIdleSweepsKeepProgress(t *testing.T) {
	j := nativeJob(t)
	j.Trigger = Trigger{Kind: "cron", Cron: "0 9 * * *"}
	j.Catchup = "skip"
	j.GraceSeconds = 60
	anchor := at(t, "2026-10-01T10:00:00Z")
	idle := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}
	for _, now := range []time.Time{
		anchor.Add(90 * time.Second),
		anchor.Add(time.Hour),
		at(t, "2026-10-02T08:59:59Z"),
	} {
		batch, e := PlanRecurring(j, definitionUID, idle, now)
		require.NoError(t, e)
		require.Empty(t, batch.Due)
		require.Equal(t, idle, batch.Progress, "no occurrence passed by %s", now)
	}
	due, e := PlanRecurring(j, definitionUID, idle, at(t, "2026-10-02T09:00:30Z"))
	require.NoError(t, e)
	require.Equal(t, []time.Time{at(t, "2026-10-02T09:00:00Z")}, due.Due)
	require.Equal(t, at(t, "2026-10-02T09:00:30Z"), due.Progress.Through)
	missed, e := PlanRecurring(j, definitionUID, idle, at(t, "2026-10-02T09:05:00Z"))
	require.NoError(t, e)
	require.Empty(t, missed.Due, "an occurrence older than grace is discarded")
	require.Equal(t, at(t, "2026-10-02T09:05:00Z"), missed.Progress.Through,
		"discarding an occurrence records progress past it")
}
