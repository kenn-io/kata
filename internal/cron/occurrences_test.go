package cron

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecurringOccurrencesMatchesPlanRecurring(t *testing.T) {
	for _, start := range []string{"2026-03-06T00:00:00Z", "2026-10-30T00:00:00Z"} {
		for _, trigger := range []Trigger{
			{Kind: "cron", Cron: "0 9 * * *"},
			{Kind: "cron", Cron: "0 12 * * *", Timezone: "America/New_York"},
			{Kind: "cron", Cron: "30 1 * * *", Timezone: "America/New_York"},
			{Kind: "cron", Cron: "30 2 * * *", Timezone: "America/New_York"},
			{Kind: "cron", Cron: "*/7 * * * *"},
			{Kind: "interval", IntervalSeconds: 7200},
		} {
			t.Run(start+trigger.Cron+trigger.Kind, func(t *testing.T) {
				anchor := at(t, start).Add(17 * time.Second)
				now := anchor.Add(5 * 24 * time.Hour)
				j := nativeJob(t)
				j.Trigger = trigger
				p := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}
				var due []time.Time
				for {
					batch, err := PlanRecurring(j, j.Issue.ParentUID, p, now)
					require.NoError(t, err)
					due = append(due, batch.Due...)
					p = batch.Progress
					if !batch.More {
						break
					}
				}
				require.GreaterOrEqual(t, len(due), 3)
				if trigger.Cron == "*/7 * * * *" {
					require.Greater(t, len(due), 50)
				}
				got, err := RecurringOccurrences(trigger, anchor, anchor, len(due))
				require.NoError(t, err)
				require.Equal(t, due, got)
			})
		}
	}
}

func TestRecurringOccurrencesAnchorFloor(t *testing.T) {
	anchor := at(t, "2026-01-01T12:00:00Z")
	for _, trigger := range []Trigger{{Kind: "interval", IntervalSeconds: 3600}, {Kind: "cron", Cron: "0 * * * *"}} {
		for _, from := range []string{"2026-01-01T09:00:00Z", "2026-01-01T11:00:00Z", "2026-01-01T11:59:59Z", "2026-01-01T12:00:00Z"} {
			got, err := RecurringOccurrences(trigger, anchor, at(t, from), 2)
			require.NoError(t, err)
			require.Equal(t, []time.Time{anchor.Add(time.Hour), anchor.Add(2 * time.Hour)}, got)
		}
	}
}
func TestRecurringOccurrencesUnsafeStoredTriggers(t *testing.T) {
	for _, tc := range []struct {
		trigger Trigger
		message string
	}{
		{Trigger{Kind: "interval"}, "interval must be positive"},
		{Trigger{Kind: "interval", IntervalSeconds: -1}, "interval must be positive"},
		{Trigger{Kind: "interval", IntervalSeconds: maxDurationSeconds + 1}, "interval exceeds supported duration"},
		{Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: "Not/AZone"}, "invalid timezone"},
		{Trigger{Kind: "manual"}, "preview supports cron, interval and once triggers; this job uses manual"},
	} {
		got, err := RecurringOccurrences(tc.trigger, at(t, "2026-01-01T00:00:00Z"), at(t, "2026-01-01T00:00:00Z"), 1)
		require.ErrorIs(t, err, ErrInvalid)
		require.ErrorContains(t, err, tc.message)
		require.Empty(t, got)
	}
}
func TestRecurringLocationLocalIsUTC(t *testing.T) {
	old := time.Local
	t.Cleanup(func() { time.Local = old })
	var err error
	time.Local, err = time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	trigger := Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: "Local"}
	loc, err := RecurringLocation(trigger)
	require.NoError(t, err)
	require.Equal(t, time.UTC, loc)
	got, err := RecurringOccurrences(trigger, time.Time{}, at(t, "2026-01-01T00:00:00Z"), 1)
	require.NoError(t, err)
	require.Equal(t, []time.Time{at(t, "2026-01-01T09:00:00Z")}, got)
}
func TestRecurringOccurrencesStrictlyAfterFrom(t *testing.T) {
	got, err := RecurringOccurrences(Trigger{Kind: "cron", Cron: "0 9 * * *"}, time.Time{}, at(t, "2026-01-01T09:00:00Z"), 1)
	require.NoError(t, err)
	require.Equal(t, []time.Time{at(t, "2026-01-02T09:00:00Z")}, got)
}
func TestRecurringOccurrencesNoNextOccurrence(t *testing.T) {
	_, err := RecurringOccurrences(Trigger{Kind: "cron", Cron: "0 0 30 2 *"}, time.Time{}, at(t, "2026-01-01T00:00:00Z"), 1)
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, "schedule has no supported next occurrence")
}
func TestRecurringOccurrencesRejectsBadArguments(t *testing.T) {
	_, err := RecurringOccurrences(Trigger{Kind: "cron", Cron: "0 9 * * *"}, time.Time{}, time.Now(), 0)
	require.ErrorContains(t, err, "occurrence count must be positive")
	_, err = RecurringOccurrences(Trigger{Kind: "interval", IntervalSeconds: 60}, time.Time{}, time.Now(), 1)
	require.ErrorContains(t, err, "interval preview requires an anchor")
}
func FuzzRecurringOccurrencesIntervalAlignment(f *testing.F) {
	f.Add(int64(3599), int64(-10800*time.Second))
	f.Add(int64(7199), int64(8000*time.Second))
	f.Add(maxDurationSeconds-1, int64(1<<63-1))
	f.Fuzz(func(t *testing.T, rawSeconds, offset int64) {
		seconds := rawSeconds % maxDurationSeconds
		if seconds < 0 {
			seconds = -seconds
		}
		seconds++
		anchor := time.Date(2026, 1, 1, 12, 0, 0, 123456789, time.UTC)
		from := anchor.Add(time.Duration(offset))
		got, err := RecurringOccurrences(Trigger{Kind: "interval", IntervalSeconds: seconds}, anchor.In(time.FixedZone("anchor-zone", 5*3600)), from.In(time.FixedZone("from-zone", -7*3600)), 1)
		if offset == 1<<63-1 {
			require.ErrorIs(t, err, ErrInvalid)
			return
		}
		require.NoError(t, err)
		elapsed := max(offset, 0)
		k := elapsed/int64(time.Duration(seconds)*time.Second) + 1
		expected := time.Unix(anchor.Unix()+k*seconds, int64(anchor.Nanosecond())).UTC()
		require.Equal(t, []time.Time{expected}, got)
	})
}

func TestRecurringOccurrencesZeroAnchorHasNoFloor(t *testing.T) {
	from := time.Date(0, time.December, 30, 0, 0, 0, 0, time.UTC)
	got, err := RecurringOccurrences(Trigger{Kind: "cron", Cron: "0 0 * * *"}, time.Time{}, from, 1)
	require.NoError(t, err)
	require.Equal(t, []time.Time{time.Date(0, time.December, 31, 0, 0, 0, 0, time.UTC)}, got)
}

func TestRecurringOccurrencesExhaustion(t *testing.T) {
	for _, tr := range []Trigger{{Kind: "interval", IntervalSeconds: 1}, {Kind: "cron", Cron: "* * * * *"}} {
		from := at(t, "9999-12-31T23:59:58Z")
		want := []time.Time{at(t, "9999-12-31T23:59:59Z")}
		if tr.Kind == "cron" {
			from = at(t, "9999-12-31T23:58:00Z")
			want = []time.Time{at(t, "9999-12-31T23:59:00Z")}
		}
		got, err := RecurringOccurrences(tr, from, from, 3)
		require.NoError(t, err)
		require.Equal(t, want, got)
		_, err = RecurringOccurrences(tr, from, want[0], 1)
		require.ErrorIs(t, err, ErrInvalid)
	}
	_, err := RecurringOccurrences(Trigger{Kind: "interval", IntervalSeconds: 60}, at(t, "2026-01-01T00:00:00Z"), at(t, "2400-01-01T00:00:00Z"), 1)
	require.ErrorContains(t, err, "schedule has no supported next occurrence")
}
func TestRecurringOccurrencesHelperBoundsAndSchedulePolicy(t *testing.T) {
	anchor := at(t, "2026-01-01T00:00:00Z")
	for _, n := range []int{-1, 0} {
		_, err := RecurringOccurrences(Trigger{Kind: "interval", IntervalSeconds: 1}, anchor, anchor, n)
		require.ErrorIs(t, err, ErrInvalid)
	}
	got, err := RecurringOccurrences(Trigger{Kind: "interval", IntervalSeconds: 1}, anchor, anchor, 101)
	require.NoError(t, err)
	require.Len(t, got, 101)
	require.Equal(t, anchor.Add(101*time.Second), got[100])
	for _, tr := range []Trigger{
		{Kind: "interval", IntervalSeconds: 60, Timezone: "Not/AZone"},
		{Kind: "cron", Cron: "@every 1h"},
		{Kind: "cron", Cron: "TZ=America/New_York 0 9 * * *", Timezone: "UTC"},
	} {
		_, err := RecurringOccurrences(tr, anchor, anchor, 1)
		require.ErrorIs(t, err, ErrInvalid)
	}
	got, err = RecurringOccurrences(Trigger{Kind: "cron", Cron: "CRON_TZ=UTC 0 9 * * *", Timezone: "UTC"}, time.Time{}, anchor, 1)
	require.NoError(t, err)
	require.Equal(t, []time.Time{at(t, "2026-01-01T09:00:00Z")}, got)
}
