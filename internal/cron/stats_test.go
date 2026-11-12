package cron

import (
	"testing"
	"time"
	_ "time/tzdata"

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
	require.Equal(t, 2, got.ByStatus["failed"])
	require.InDelta(t, 0.3333, *got.SuccessRate, 0.00001)
	require.Equal(t, 20.0, got.DurationSeconds.Median)
	require.Len(t, got.Buckets, 3)
	require.Equal(t, "2026-03-09", got.Buckets[0].Date)
	empty := SummarizeRuns(nil, false, loc)
	require.Nil(t, empty.SuccessRate)
	require.Nil(t, empty.DurationSeconds)
	require.Nil(t, empty.Buckets)
}
