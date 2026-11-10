package cron

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseCronDirectiveWithoutScheduleDoesNotPanic(t *testing.T) {
	for _, expression := range []string{"TZ=UTC", "CRON_TZ=UTC", "TZ=UTC\t0\t9\t*\t*\t*"} {
		t.Run(expression, func(t *testing.T) {
			require.NotPanics(t, func() {
				_, err := ParseCron(expression)
				require.Error(t, err)
			})
			require.NotPanics(t, func() {
				require.ErrorIs(t, (Trigger{Kind: "cron", Cron: expression}).Validate(), ErrInvalid)
			})
			j := nativeJob(t)
			j.Trigger = Trigger{Kind: "cron", Cron: expression}
			require.NotPanics(t, func() { require.ErrorIs(t, j.Validate(), ErrInvalid) })
		})
	}
}

func TestPlanRecurringRejectsDirectiveWithoutSchedule(t *testing.T) {
	j := nativeJob(t)
	j.Trigger = Trigger{Kind: "cron", Cron: "TZ=UTC"}
	encoded, err := json.Marshal(j)
	require.NoError(t, err)
	stored, err := DecodeJob(encoded)
	require.NoError(t, err)
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}
	require.NotPanics(t, func() {
		batch, err := PlanRecurring(stored, definitionUID, p, anchor.Add(time.Hour))
		require.ErrorIs(t, err, ErrInvalid)
		require.Empty(t, batch.Due)
		require.Equal(t, p, batch.Progress)
	})
}

// Valid directives retain the shared grammar for adapter and native jobs.
func TestCronCompatibilityTimezoneDirectives(t *testing.T) {
	for _, tc := range []struct{ expression, zone string }{
		{"TZ=UTC 0 9 * * *", "UTC"},
		{"CRON_TZ=America/New_York 0 9 * * 1-5", "America/New_York"},
		{"TZ=UTC @daily", "UTC"},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			_, err := ParseCron(tc.expression)
			require.NoError(t, err)
			adapter, err := ParseJob([]byte(executeJSON))
			require.NoError(t, err)
			adapter.Trigger = Trigger{Kind: "cron", Cron: tc.expression, Timezone: tc.zone}
			require.NoError(t, adapter.Validate())
			native := nativeJob(t)
			native.Trigger = adapter.Trigger
			require.NoError(t, native.Validate())
		})
	}
}

func FuzzParseCronDirectiveDoesNotPanic(f *testing.F) {
	for _, expression := range []string{"TZ=UTC", "CRON_TZ=UTC", "TZ=UTC\t0 9 * * *", "TZ=UTC @daily"} {
		f.Add(expression)
	}
	f.Fuzz(func(_ *testing.T, expression string) { _, _ = ParseCron(expression) })
}

func TestCronDirectiveGuardPreservesLeadingWhitespaceRejection(t *testing.T) {
	for _, expression := range []string{" @daily", "\tTZ=UTC @daily"} {
		_, err := ParseCron(expression)
		require.Error(t, err)
	}
}
