package cron

import (
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func previewJob(t testing.TB) JobDefinition {
	j := adapterJob(t, Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: "America/New_York"})
	j.Catchup = "latest"
	return j
}

// A document that fails strict decoding has no normalized document.
func TestCronValidationDecodeShapes(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `1`, `{"unknown":1}`, `{`} {
		_, normalized, err := Normalize([]byte(raw), DecodeJob)
		require.Error(t, err)
		require.Empty(t, normalized)
	}
}

func TestCronPreviewNativeIntervals(t *testing.T) {
	{
		job := previewJob(t)
		job.Action = Action{Kind: "notify", Recipient: "current-owner-or-author"}
		job.Issue.ParentUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		job.Overlap = "allow"
		job.Trigger = Trigger{Kind: "interval", IntervalSeconds: 60}
		out, err := PreviewSchedule(job, nil, time.Time{}, 1)
		require.NoError(t, err)
		require.Len(t, out.Next, 1)
		require.Equal(t, time.Time{}.Add(time.Minute), out.Next[0].At)

	}
	hegel.Test(t, func(ht *hegel.T) {
		seconds := int64(hegel.Draw(ht, hegel.Integers(60, 1000000)))
		count := hegel.Draw(ht, hegel.Integers(1, 100))
		from := time.Unix(int64(hegel.Draw(ht, hegel.Integers(0, 2000000000))), 0).UTC()
		job := previewJob(ht)
		job.Action = Action{Kind: "notify", Recipient: "current-owner-or-author"}
		job.Issue.ParentUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		job.Overlap = "allow"
		job.Trigger = Trigger{Kind: "interval", IntervalSeconds: seconds}
		job.Enabled = false
		out, err := PreviewSchedule(job, nil, from, count)
		require.NoError(ht, err)
		require.Equal(ht, "fresh_anchor", out.Basis)
		require.Equal(ht, "daemon", out.Delivery)
		require.Len(ht, out.Next, count)
		for i, fire := range out.Next {
			require.Equal(ht, from.Add(time.Duration(i+1)*time.Duration(seconds)*time.Second), fire.At)
		}

	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}

func TestCronPreviewBasesAndZones(t *testing.T) {
	from := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	job := previewJob(t)
	out, err := PreviewSchedule(job, nil, from, 3)
	require.NoError(t, err)
	require.Equal(t, "adapter_trigger", out.Basis)
	require.Equal(t, "2026-10-31T09:00:00-04:00", out.Next[0].Local)
	require.Equal(t, "2026-11-01T09:00:00-05:00", out.Next[1].Local)
	for _, trigger := range []Trigger{{Kind: "interval", IntervalSeconds: 60}, {Kind: "cron", Cron: "@every 1h"}, {Kind: "manual"}} {
		job.Trigger = trigger
		out, err = PreviewSchedule(job, nil, from, 5)
		require.NoError(t, err)
		require.Empty(t, out.Next)
		if trigger.Kind == "manual" {
			require.Equal(t, "none", out.Basis)
		} else {
			require.Equal(t, "adapter_defined", out.Basis)
		}
	}
	job.Trigger = Trigger{Kind: "once", At: from.Add(time.Hour).Format(time.RFC3339)}
	out, err = PreviewSchedule(job, nil, from, 100)
	require.NoError(t, err)
	require.Len(t, out.Next, 1)
	for _, count := range []int{0, 101} {
		_, err = PreviewSchedule(job, nil, from, count)
		require.Error(t, err)
	}
}

// RFC3339 permits the Go zero instant; a hypothetical anchor is not persisted
// progress and must not inherit the stored-progress sentinel restriction.
