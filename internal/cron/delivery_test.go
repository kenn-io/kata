package cron

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestDescribeRecurring(t *testing.T) {
	anchor := at(t, "2026-10-01T12:00:00Z")
	for _, tc := range []struct {
		name, expression, catchup            string
		elapsed                              time.Duration
		count                                int
		next                                 time.Time
		disabled, badProgress, invalid, more bool
	}{
		{name: "daily all", expression: "0 9 * * *", catchup: "all", elapsed: 72 * time.Hour, count: 3, next: at(t, "2026-10-02T09:00:00Z")},
		{name: "interval", catchup: "all", elapsed: 5 * time.Hour, count: 2, next: anchor.Add(2 * time.Hour)},
		{name: "impossible", expression: "0 0 30 2 *", catchup: "all", elapsed: time.Hour, invalid: true},
		{name: "bad progress", catchup: "all", elapsed: time.Hour, badProgress: true, invalid: true},
		{name: "disabled", catchup: "all", elapsed: 5 * time.Hour, count: 2, next: anchor.Add(2 * time.Hour), disabled: true},
		{name: "idle", expression: "0 9 * * *", catchup: "all", elapsed: time.Hour, next: at(t, "2026-10-02T09:00:00Z")},
		{name: "latest scan", expression: "0 * * * *", catchup: "latest", elapsed: 60 * 24 * time.Hour, more: true, next: anchor.Add(time.Hour)},
		{name: "skip grace", expression: "0 * * * *", catchup: "skip", elapsed: 72 * time.Hour, count: 1, next: anchor.Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := nativeJob(t)
			j.Catchup = tc.catchup
			j.Enabled = !tc.disabled
			if tc.expression != "" {
				j.Trigger = Trigger{Kind: "cron", Cron: tc.expression}
			}
			if tc.catchup == "skip" {
				j.GraceSeconds = 600
			}
			p := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}
			if tc.badProgress {
				p.Version = 2
			}
			d, e := DescribeRecurring(j, definitionUID, p, anchor.Add(tc.elapsed))
			if tc.invalid {
				require.ErrorIs(t, e, ErrInvalid)
				require.True(t, d.NextDueAt.IsZero())
				if tc.badProgress {
					require.ErrorContains(t, e, "invalid recurring progress")
				}
				return
			}
			require.NoError(t, e)
			require.Equal(t, tc.next, d.NextDueAt)
			require.Len(t, d.Batch.Due, tc.count)
			require.Equal(t, tc.more, d.Batch.More)
			if tc.more {
				require.NotNil(t, d.Batch.Progress.Scan)
			}
			if tc.catchup == "skip" {
				require.Equal(t, anchor.Add(tc.elapsed), d.Batch.Due[0])
			}
		})
	}
}

func TestDescribeRecurringAfterDelivery(t *testing.T) {
	j := nativeJob(t)
	j.Trigger = Trigger{Kind: "cron", Cron: "0 9 * * *"}
	anchor := at(t, "2026-10-01T12:00:00Z")
	p := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor.Add(72 * time.Hour)}
	d, err := DescribeRecurring(j, definitionUID, p, p.Through.Add(30*time.Second))
	require.NoError(t, err)
	require.Equal(t, at(t, "2026-10-05T09:00:00Z"), d.NextDueAt)
	require.True(t, d.NextDueAt.After(p.Through))
	require.Empty(t, d.Batch.Due)
}

func TestDescribeRecurringIntervalProperty(t *testing.T) {
	base := nativeJob(t)
	rapid.Check(t, func(t *rapid.T) {
		seconds := rapid.Int64Range(60, 86400).Draw(t, "seconds")
		elapsed := rapid.Int64Range(0, 1000000).Draw(t, "elapsed")
		anchor := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		j := base
		j.Trigger.IntervalSeconds = seconds
		p := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor.Add(time.Duration(elapsed) * time.Second)}
		now := p.Through.Add(10 * time.Hour)
		d, e := DescribeRecurring(j, definitionUID, p, now)
		require.NoError(t, e)
		b, e := PlanRecurring(j, definitionUID, p, now)
		require.NoError(t, e)
		require.Equal(t, b, d.Batch)
		require.Equal(t, anchor.Add(time.Duration((elapsed/seconds+1)*seconds)*time.Second), d.NextDueAt)
	})
}

func TestDescribeRecurringIntervalPhaseProperty(t *testing.T) {
	base := nativeJob(t)
	rapid.Check(t, func(t *rapid.T) {
		seconds := rapid.Int64Range(60, maxDurationSeconds).Draw(t, "seconds")
		offset := rapid.Int64Range(0, 1<<63-1).Draw(t, "offset")
		j := base
		j.Enabled = rapid.Bool().Draw(t, "enabled")
		j.Trigger.IntervalSeconds = seconds
		anchor := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		p := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor.Add(time.Duration(offset))}
		beforeJob, err := json.Marshal(j)
		require.NoError(t, err)
		beforeProgress, err := json.Marshal(p)
		require.NoError(t, err)
		d, err := DescribeRecurring(j, definitionUID, p, p.Through)
		if offset == 1<<63-1 {
			require.ErrorIs(t, err, ErrInvalid)
			require.True(t, d.NextDueAt.IsZero())
		} else {
			require.NoError(t, err)
			steps := offset/(seconds*int64(time.Second)) + 1
			require.Equal(t, time.Unix(anchor.Unix()+steps*seconds, 0).UTC(), d.NextDueAt)
			require.Empty(t, d.Batch.Due)
		}
		afterJob, err := json.Marshal(j)
		require.NoError(t, err)
		afterProgress, err := json.Marshal(p)
		require.NoError(t, err)
		require.Equal(t, string(beforeJob), string(afterJob))
		require.Equal(t, string(beforeProgress), string(afterProgress))
	})
}
