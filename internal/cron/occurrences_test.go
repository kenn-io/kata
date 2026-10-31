package cron

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// delivered replays the daemon's planner with an all-catchup clock at until and
// returns every occurrence it would deliver after progress.Through.
func delivered(t *testing.T, j JobDefinition, p RecurringProgress, until time.Time) []time.Time {
	t.Helper()
	j.Catchup = "all"
	var out []time.Time
	for {
		batch, err := PlanRecurring(j, definitionUID, p, until)
		require.NoError(t, err)
		out = append(out, batch.Due...)
		p = batch.Progress
		if !batch.More {
			return out
		}
	}
}

func times(t *testing.T, values ...string) []time.Time {
	t.Helper()
	out := make([]time.Time, len(values))
	for i, v := range values {
		out[i] = at(t, v)
	}
	return out
}

func TestRecurringOccurrencesMatchDaemonDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, cron, zone, after string
		want                    []string
	}{
		{
			name: "spring forward gap runs an hour later", cron: "30 2 * * *", zone: "America/New_York", after: "2026-03-07T08:00:00Z",
			want: []string{"2026-03-08T07:30:00Z", "2026-03-09T06:30:00Z", "2026-03-10T06:30:00Z"},
		},
		{
			name: "fall back repeated hour runs twice", cron: "30 1 * * *", zone: "America/New_York", after: "2026-11-01T04:00:00Z",
			want: []string{"2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z", "2026-11-02T06:30:00Z"},
		},
		{
			name: "hourly through the repeat", cron: "0 * * * *", zone: "America/New_York", after: "2026-11-01T04:59:00Z",
			want: []string{"2026-11-01T05:00:00Z", "2026-11-01T06:00:00Z", "2026-11-01T07:00:00Z"},
		},
		{name: "seven is Sunday", cron: "0 9 * * 7", after: "2026-10-12T00:00:00Z", want: []string{"2026-10-18T09:00:00Z", "2026-10-25T09:00:00Z", "2026-11-01T09:00:00Z"}},
		{name: "wrapping range", cron: "0 9 * * 6-1", after: "2026-10-15T00:00:00Z", want: []string{"2026-10-17T09:00:00Z", "2026-10-18T09:00:00Z", "2026-10-19T09:00:00Z"}},
		{name: "five slash two is every other day from five", cron: "0 9 * * 5/2", after: "2026-10-12T00:00:00Z", want: []string{"2026-10-16T09:00:00Z", "2026-10-18T09:00:00Z", "2026-10-23T09:00:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := nativeJob(t)
			j.Trigger = Trigger{Kind: "cron", Cron: tc.cron, Timezone: tc.zone}
			after := at(t, tc.after)
			p := RecurringProgress{Version: 1, Anchor: after, Through: after}
			got, err := RecurringOccurrences(j, p, after, 3)
			require.NoError(t, err)
			require.Equal(t, times(t, tc.want...), got)
			require.Equal(t, got, delivered(t, j, p, got[2])[:3], "daemon delivery must match the preview")
		})
	}
}

func TestRecurringOccurrencesLordHowe(t *testing.T) {
	// Lord Howe shifts by 30 minutes: DST began 2026-10-04 (02:00 to 02:30, so
	// a daily 02:15 has no instant that day) and ended 2026-04-05 (02:00 back
	// to 01:30, so 01:45 happens twice). Expected instants are worked out by hand
	// from +10:30 standard and +11:00 daylight offsets.
	for _, tc := range []struct {
		name, cron, after string
		want              []string
	}{
		{"skipped half hour", "15 2 * * *", "2026-10-02T12:00:00Z", []string{"2026-10-02T15:45:00Z", "2026-10-04T15:15:00Z", "2026-10-05T15:15:00Z"}},
		{"repeated half hour", "45 1 * * *", "2026-04-03T12:00:00Z", []string{"2026-04-03T14:45:00Z", "2026-04-04T14:45:00Z", "2026-04-04T15:15:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := nativeJob(t)
			j.Trigger = Trigger{Kind: "cron", Cron: tc.cron, Timezone: "Australia/Lord_Howe"}
			after := at(t, tc.after)
			p := RecurringProgress{Version: 1, Anchor: after, Through: after}
			got, err := RecurringOccurrences(j, p, after, 3)
			require.NoError(t, err)
			require.Equal(t, times(t, tc.want...), got)
			require.Equal(t, got, delivered(t, j, p, got[2])[:3])
		})
	}
}

func TestRecurringOccurrencesAdapterCronGrammar(t *testing.T) {
	after := at(t, "2026-03-07T08:00:00Z")
	// An adapter's own directive stands alone, and a conflicting trigger zone
	// is not a native-only error.
	got, err := RecurringOccurrences(adapterJob(t, Trigger{Kind: "cron", Cron: "CRON_TZ=Europe/Paris 0 9 * * *"}), RecurringProgress{}, after, 2)
	require.NoError(t, err)
	require.Equal(t, times(t, "2026-03-08T08:00:00Z", "2026-03-09T08:00:00Z"), got)
	got, err = RecurringOccurrences(adapterJob(t, Trigger{Kind: "cron", Cron: "TZ=Europe/Paris 0 9 * * *", Timezone: "UTC"}), RecurringProgress{}, after, 1)
	require.NoError(t, err)
	require.Equal(t, times(t, "2026-03-08T08:00:00Z"), got)
	// @every is an interval and carries no instant.
	for _, expression := range []string{"@every 1h", "CRON_TZ=UTC @every 1h"} {
		got, err = RecurringOccurrences(adapterJob(t, Trigger{Kind: "cron", Cron: expression}), RecurringProgress{}, after, 3)
		require.NoError(t, err)
		require.Empty(t, got, expression)
	}
}

func TestTriggerLocation(t *testing.T) {
	require.Equal(t, "UTC", TriggerLocation(Trigger{Kind: "cron", Cron: "0 9 * * *"}).String())
	require.Equal(t, "America/New_York", TriggerLocation(Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: "America/New_York"}).String())
	require.Equal(t, "Europe/Paris", TriggerLocation(Trigger{Kind: "cron", Cron: "CRON_TZ=Europe/Paris 0 9 * * *"}).String())
	require.Equal(t, "Europe/Paris", TriggerLocation(Trigger{Kind: "cron", Cron: "TZ=Europe/Paris 0 9 * * *", Timezone: "UTC"}).String())
	require.Equal(t, "Asia/Tokyo", TriggerLocation(Trigger{Kind: "once", At: "2026-04-01T12:00:00Z", Timezone: "Asia/Tokyo"}).String())
}

func TestRecurringOccurrencesNativeAnchor(t *testing.T) {
	j := nativeJob(t) // every 7200 seconds
	anchor := at(t, "2026-06-01T00:30:00Z")
	p := RecurringProgress{Version: 1, Anchor: anchor, Through: at(t, "2026-06-01T04:30:00Z")}
	now := at(t, "2026-06-01T05:00:00Z")
	got, err := RecurringOccurrences(j, p, now, 3)
	require.NoError(t, err)
	require.Equal(t, times(t, "2026-06-01T06:30:00Z", "2026-06-01T08:30:00Z", "2026-06-01T10:30:00Z"), got)
	require.Equal(t, got, delivered(t, j, p, got[2])[:3])
}

func TestRecurringOccurrencesSkipsNothingAlreadyDelivered(t *testing.T) {
	j := nativeJob(t)
	anchor := at(t, "2026-06-01T00:00:00Z")
	// Through is ahead of the preview clock: delivered occurrences are not previewed.
	p := RecurringProgress{Version: 1, Anchor: anchor, Through: at(t, "2026-06-01T06:00:00Z")}
	got, err := RecurringOccurrences(j, p, at(t, "2026-06-01T01:00:00Z"), 1)
	require.NoError(t, err)
	require.Equal(t, times(t, "2026-06-01T08:00:00Z"), got)
}

func adapterJob(t *testing.T, trigger Trigger) JobDefinition {
	t.Helper()
	j, err := ParseJob([]byte(executeJSON))
	require.NoError(t, err)
	j.Trigger = trigger
	j.Enabled = true
	return j
}

func TestRecurringOccurrencesAdapterTriggers(t *testing.T) {
	after := at(t, "2026-03-07T08:00:00Z")
	got, err := RecurringOccurrences(adapterJob(t, Trigger{Kind: "cron", Cron: "30 2 * * *", Timezone: "America/New_York"}), RecurringProgress{}, after, 3)
	require.NoError(t, err)
	require.Equal(t, times(t, "2026-03-08T07:30:00Z", "2026-03-09T06:30:00Z", "2026-03-10T06:30:00Z"), got)

	got, err = RecurringOccurrences(adapterJob(t, Trigger{Kind: "once", At: "2026-04-01T12:00:00+02:00"}), RecurringProgress{}, after, 3)
	require.NoError(t, err)
	require.Equal(t, times(t, "2026-04-01T10:00:00Z"), got)

	got, err = RecurringOccurrences(adapterJob(t, Trigger{Kind: "once", At: "2026-03-01T00:00:00Z"}), RecurringProgress{}, after, 3)
	require.NoError(t, err)
	require.Empty(t, got, "a past once trigger has no next time")

	// Kata holds no anchor, lease, or catch-up for an adapter interval.
	got, err = RecurringOccurrences(adapterJob(t, Trigger{Kind: "interval", IntervalSeconds: 3600}), RecurringProgress{}, after, 3)
	require.NoError(t, err)
	require.Empty(t, got)

	for _, trigger := range []Trigger{{Kind: "manual"}, {Kind: "issue-deadline", IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", LeadSeconds: 60}} {
		got, err = RecurringOccurrences(adapterJob(t, trigger), RecurringProgress{}, after, 3)
		require.NoError(t, err)
		require.Empty(t, got)
	}
}

func TestRecurringOccurrencesPausedJobHasNone(t *testing.T) {
	anchor := at(t, "2026-06-01T00:00:00Z")
	native := nativeJob(t)
	adapter := adapterJob(t, Trigger{Kind: "cron", Cron: "0 9 * * *"})
	for name, j := range map[string]JobDefinition{"native": native, "adapter": adapter} {
		j.Enabled = false
		got, err := RecurringOccurrences(j, RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}, anchor, 3)
		require.NoError(t, err, name)
		require.Empty(t, got, name)
	}
}

func TestRecurringNextEmptyDirectiveZoneConflicts(t *testing.T) {
	// Trigger.Validate also refuses these, so reach the native step directly:
	// it must not accept an embedded zone that differs from the trigger's.
	for _, expression := range []string{"TZ= 0 9 * * *", "CRON_TZ= 0 9 * * *"} {
		_, err := recurringNext(Trigger{Kind: "cron", Cron: expression}, time.Time{})
		require.ErrorIs(t, err, ErrInvalid, expression)
		require.ErrorContains(t, err, "conflicting cron timezone", expression)
	}
}

func TestRecurringOccurrencesAdapterEmptyDirectiveMirrorsParseCron(t *testing.T) {
	after := at(t, "2026-03-07T08:00:00Z")
	for _, tc := range []Trigger{{Kind: "cron", Cron: "TZ= 0 9 * * *"}, {Kind: "cron", Cron: "CRON_TZ= 0 9 * * *", Timezone: "UTC"}} {
		_, parseErr := ParseCron(tc.Cron)
		got, err := RecurringOccurrences(adapterJob(t, tc), RecurringProgress{}, after, 1)
		if parseErr != nil {
			require.Error(t, err, tc.Cron)
			require.Empty(t, got)
			continue
		}
		require.NoError(t, err)
		require.NotEmpty(t, got, tc.Cron)
	}
}

func TestRecurringOccurrencesRejectsInvalidNativeJob(t *testing.T) {
	j := nativeJob(t)
	j.Trigger = Trigger{Kind: "interval"}
	anchor := at(t, "2026-06-01T00:00:00Z")
	p := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}
	require.NotPanics(t, func() {
		got, err := RecurringOccurrences(j, p, anchor, 3)
		require.ErrorIs(t, err, ErrInvalid)
		require.Empty(t, got)
	})
}
