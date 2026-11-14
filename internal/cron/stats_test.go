package cron

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSummarizeRuns(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	at := func(s string) time.Time { v, e := time.Parse(time.RFC3339, s); require.NoError(t, e); return v }
	start := at("2026-03-08T06:30:00Z")
	end := start.Add(20 * time.Second)
	rows := []RunStatRow{
		{Status: "failed", CreatedAt: at("2026-03-09T04:30:00Z"), StartedAt: &start, EndedAt: &end},
		{Status: "running", CreatedAt: at("2026-03-08T07:30:00Z")},
		{Status: "failed", CreatedAt: at("2026-03-08T06:30:00Z")},
		{Status: "succeeded", CreatedAt: at("2026-03-07T12:00:00Z")},
	}
	got := SummarizeRuns(rows, true, loc)
	require.Equal(t, 2, got.ConsecutiveFailures)
	require.InDelta(t, 0.3333, *got.SuccessRate, 0.00001)
	require.Equal(t, 20.0, got.DurationSeconds.Median)
	require.Len(t, got.Buckets, 2)
	require.Equal(t, "2026-03-08", got.Buckets[0].Date)
	require.Equal(t, 20.0, *got.Buckets[0].MedianDurationSeconds)
	require.Equal(t, "2026-03-07", got.Buckets[1].Date)
	require.Nil(t, got.Buckets[1].MedianDurationSeconds)
	require.Equal(t, map[string]int{"running": 1, "failed": 2}, got.Buckets[0].ByStatus)
	empty := SummarizeRuns(nil, false, loc)
	require.Nil(t, empty.DurationSeconds)
	require.Nil(t, empty.Buckets)
}

// Every status contributes to counts; only success and failure enter the rate.
func TestSummarizeRunsStatusMixes(t *testing.T) {
	statuses := []string{"running", "succeeded", "failed", "cancelled", "unknown"}
	instant := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for mask := 0; mask < 1<<len(statuses); mask++ {
		var rows []RunStatRow
		for i, status := range statuses {
			if mask&(1<<i) != 0 {
				rows = append(rows, RunStatRow{Status: status, CreatedAt: instant})
			}
		}
		got := SummarizeRuns(rows, false, time.UTC)
		for i, status := range statuses {
			expected := 0
			if mask&(1<<i) != 0 {
				expected = 1
			}
			require.Equal(t, expected, got.ByStatus[status])
		}
		switch mask & 6 {
		case 0:
			require.Nil(t, got.SuccessRate)
		case 2:
			require.Equal(t, 1.0, *got.SuccessRate)
		case 4:
			require.Equal(t, 0.0, *got.SuccessRate)
		case 6:
			require.Equal(t, 0.5, *got.SuccessRate)
		}
	}
}

func TestSummarizeRunsNearestRank(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var rows []RunStatRow
	for i := 1; i <= 20; i++ {
		end := start.Add(time.Duration(i) * time.Second)
		rows = append(rows, RunStatRow{Status: "succeeded", CreatedAt: start, StartedAt: &start, EndedAt: &end})
	}
	got := SummarizeRuns(rows, false, time.UTC)
	require.Equal(t, 10.0, got.DurationSeconds.Median)
	require.Equal(t, 19.0, got.DurationSeconds.P95)
}

func TestSummarizeRunsLastCompletionWithOverlaps(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	later := base.Add(10 * time.Minute)
	earlier := base.Add(5 * time.Minute)
	rows := []RunStatRow{
		{Status: "succeeded", CreatedAt: base.Add(4 * time.Minute), EndedAt: &earlier},
		{Status: "failed", CreatedAt: base.Add(3 * time.Minute)},
		{Status: "succeeded", CreatedAt: base.Add(2 * time.Minute), EndedAt: &later},
		{Status: "failed", CreatedAt: base, EndedAt: &later},
	}
	got := SummarizeRuns(rows, false, time.UTC)
	require.Equal(t, later, *got.LastSucceededAt)
	require.Equal(t, later, *got.LastFailedAt)
}

func TestSummarizeRunsLongDuration(t *testing.T) {
	start := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC)
	got := SummarizeRuns([]RunStatRow{{Status: "succeeded", CreatedAt: start, StartedAt: &start, EndedAt: &end}}, true, time.UTC)
	require.Equal(t, float64(end.Unix()-start.Unix()), got.DurationSeconds.Median)
	require.Equal(t, got.DurationSeconds.Median, *got.Buckets[0].MedianDurationSeconds)
}

func TestSummarizeRunsCompletionOrder(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start := base.Add(time.Hour)
	failEnd := base.Add(2 * time.Hour)
	successEnd := base.Add(3 * time.Hour)
	rows := []RunStatRow{{Status: "failed", CreatedAt: base.Add(24 * time.Hour), StartedAt: &start, EndedAt: &failEnd}, {Status: "succeeded", CreatedAt: base, EndedAt: &successEnd}}
	got := SummarizeRuns(rows, true, time.UTC)
	require.Zero(t, got.ConsecutiveFailures)
	require.Equal(t, "2026-01-01", got.Buckets[0].Date)
	rows[0].EndedAt = &successEnd
	require.Equal(t, 1, SummarizeRuns(rows, false, time.UTC).ConsecutiveFailures)
	rows[0].EndedAt = nil
	got = SummarizeRuns(rows, false, time.UTC)
	require.Equal(t, start, *got.LastFailedAt)
	require.Zero(t, got.ConsecutiveFailures)
}
