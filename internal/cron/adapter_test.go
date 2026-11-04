package cron

import (
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
)

// Profile v1 pins creation anchors and the shared parser, without changing previews.
func TestAdapterSchedule(t *testing.T) {
	anchor := at(t, "2030-01-01T00:00:00.5Z")
	for _, tc := range []struct {
		name    string
		trigger Trigger
		cursor  time.Time
		want    string
	}{
		{"interval before anchor", Trigger{Kind: "interval", IntervalSeconds: 3600}, anchor.Add(-time.Hour), "2030-01-01T01:00:00.5Z"},
		{"interval at anchor", Trigger{Kind: "interval", IntervalSeconds: 3600}, anchor, "2030-01-01T01:00:00.5Z"},
		{"interval after anchor", Trigger{Kind: "interval", IntervalSeconds: 3600}, anchor.Add(90 * time.Minute), "2030-01-01T02:00:00.5Z"},
		{"every anchored", Trigger{Kind: "cron", Cron: "@every 90m"}, anchor, "2030-01-01T01:30:00.5Z"},
		{"every truncates fractions", Trigger{Kind: "cron", Cron: "@every 1500ms"}, anchor, "2030-01-01T00:00:01.5Z"},
		{"directive wins", Trigger{Kind: "cron", Cron: "TZ=UTC 0 9 * * *", Timezone: "America/New_York"}, anchor, "2030-01-01T09:00:00Z"},
		{"once", Trigger{Kind: "once", At: "2030-01-01T02:00:00Z"}, anchor, "2030-01-01T02:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, err := AdapterSchedule(tc.trigger, anchor)
			require.NoError(t, err)
			require.Equal(t, at(t, tc.want), next(tc.cursor))
		})
	}
	for _, trigger := range []Trigger{{Kind: "once", At: anchor.Format(time.RFC3339Nano)}, {Kind: "once", At: anchor.Add(-time.Second).Format(time.RFC3339Nano)}} {
		next, err := AdapterSchedule(trigger, anchor)
		require.NoError(t, err)
		require.True(t, next(anchor.Add(-time.Hour)).IsZero())
	}
	next, err := AdapterSchedule(Trigger{Kind: "interval", IntervalSeconds: 3600}, anchor)
	require.NoError(t, err)
	require.Equal(t, at(t, "9999-12-31T23:00:00.5Z"), next(at(t, "9999-12-31T23:00:00Z")), "valid distant anchors must retain their grid")
	for _, trigger := range []Trigger{{Kind: "manual"}, {Kind: "interval"}, {Kind: "cron", Cron: "invalid"}} {
		_, err := AdapterSchedule(trigger, anchor)
		require.Error(t, err)
	}
	_, err = AdapterSchedule(Trigger{Kind: "interval", IntervalSeconds: 60}, time.Time{})
	require.Error(t, err)
}

func TestAdapterScheduleDST(t *testing.T) {
	next, err := AdapterSchedule(Trigger{Kind: "cron", Cron: "30 1 * * *", Timezone: "America/New_York"}, at(t, "2026-01-01T00:00:00Z"))
	require.NoError(t, err)
	first := next(at(t, "2026-11-01T04:00:00Z"))
	require.Equal(t, at(t, "2026-11-01T05:30:00Z"), first)
	require.Equal(t, at(t, "2026-11-01T06:30:00Z"), next(first))
	next, err = AdapterSchedule(Trigger{Kind: "cron", Cron: "30 2 * * *", Timezone: "America/New_York"}, at(t, "2026-01-01T00:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, at(t, "2026-03-08T07:30:00Z"), next(at(t, "2026-03-08T05:00:00Z")))
}
