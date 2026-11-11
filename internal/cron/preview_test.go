package cron

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreviewJobStates(t *testing.T) {
	now := at(t, "2026-06-01T01:00:00Z")
	progress := RecurringProgress{Version: 1, Anchor: at(t, "2026-06-01T00:00:00Z"), Through: at(t, "2026-06-01T06:00:00Z")}
	for _, tc := range []struct {
		name, status            string
		parent                  *PreviewParent
		disabled, newer, legacy bool
	}{
		{"ready", "ready", &PreviewParent{Open: true, Progress: &progress}, false, false, false},
		{"missing parent", "unavailable", nil, false, false, false},
		{"missing progress", "unavailable", &PreviewParent{Open: true}, false, false, false},
		{"closed without progress", "unavailable", &PreviewParent{Open: false}, false, false, false},
		{"closed", "parent_closed", &PreviewParent{Open: false, Progress: &progress}, false, false, false},
		{"paused", "paused", nil, true, false, false},
		{"paused newer", "", nil, true, true, false},
		{"closed legacy", "parent_closed", &PreviewParent{Open: false, Progress: &progress}, false, false, true},
		{"paused legacy", "paused", nil, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := nativeJob(t)
			job.Enabled = !tc.disabled
			if tc.newer {
				job.Version = 2
			}
			if tc.legacy {
				job.Trigger = Trigger{Kind: "cron", Cron: "0 0 30 2 *"}
			}
			got, err := PreviewJob(job, tc.parent, now, 1)
			if tc.newer {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.status, got.Status)
			if tc.status == "ready" {
				require.Equal(t, times(t, "2026-06-01T08:00:00Z"), got.Times)
			} else {
				require.Empty(t, got.Times)
			}
		})
	}
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
		{"local timezone", Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: "Local"}, "unavailable"},
		{"local directive", Trigger{Kind: "cron", Cron: "TZ=Local 0 9 * * *"}, "unavailable"},
		{"explicit zone", Trigger{Kind: "cron", Cron: "TZ=UTC 0 9 * * *", Timezone: "Local"}, "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PreviewJob(adapterJob(t, tc.trigger), nil, now, 3)
			require.NoError(t, err)
			require.Equal(t, tc.status, got.Status)
		})
	}
}
