package cron

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreviewJobStates(t *testing.T) {
	now := at(t, "2026-06-01T01:00:00Z")
	j := nativeJob(t)
	p := RecurringProgress{Version: 1, Anchor: at(t, "2026-06-01T00:00:00Z"), Through: at(t, "2026-06-01T06:00:00Z")}
	got, err := PreviewJob(j, &PreviewParent{Open: true, Progress: &p}, now, 1)
	require.NoError(t, err)
	require.Equal(t, "ready", got.Status)
	require.Equal(t, times(t, "2026-06-01T08:00:00Z"), got.Times)
	got, err = PreviewJob(j, nil, now, 1)
	require.NoError(t, err)
	require.Equal(t, "unavailable", got.Status)
	invalid := p
	invalid.Version = 2
	got, err = PreviewJob(j, &PreviewParent{Open: true, Progress: &invalid}, now, 1)
	require.NoError(t, err)
	require.Equal(t, "unavailable", got.Status)
	got, err = PreviewJob(j, &PreviewParent{Open: false}, now, 1)
	require.NoError(t, err)
	require.Equal(t, "parent_closed", got.Status)
	j.Enabled = false
	got, err = PreviewJob(j, nil, now, 1)
	require.NoError(t, err)
	require.Equal(t, "paused", got.Status)
	j.Version = 2
	_, err = PreviewJob(j, nil, now, 1)
	require.Error(t, err)
	job := nativeJob(t)
	job.Trigger = Trigger{Kind: "cron", Cron: "0 0 30 2 *"}
	for _, tc := range []struct {
		name   string
		parent *PreviewParent
		want   string
	}{
		{"missing", nil, "unavailable"},
		{"closed", &PreviewParent{Open: false}, "parent_closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PreviewJob(job, tc.parent, now, 3)
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Status)
		})
	}
	job.Enabled = false
	got, err = PreviewJob(job, nil, now, 1)
	require.NoError(t, err)
	require.Equal(t, "paused", got.Status)
}

func TestPreviewJobAdapterStates(t *testing.T) {
	now := at(t, "2026-06-01T01:00:00Z")
	for _, tc := range []struct {
		name    string
		trigger Trigger
		status  string
	}{
		{"once exhausted", Trigger{Kind: "once", At: "2026-05-01T00:00:00Z"}, "exhausted"},
		{"adapter interval", Trigger{Kind: "interval", IntervalSeconds: 3600}, "unsupported"},
		{"adapter every", Trigger{Kind: "cron", Cron: "@every 1h"}, "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PreviewJob(adapterJob(t, tc.trigger), nil, now, 3)
			require.NoError(t, err)
			require.Equal(t, tc.status, got.Status)
		})
	}
}
