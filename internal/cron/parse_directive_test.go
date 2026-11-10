package cron

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCronDiagnosticOutputSafety(t *testing.T) {
	for _, input := range []string{"x\x00y 9 * * *", "\x1b[31m 9 * * *", "x\x7fy 9 * * *", string([]byte{0xff}) + " 9 * * *", "bad\x00zone", strings.Repeat("\x00", 1000), strings.Repeat("x", 1000) + " 9 * * *", strings.Repeat("é", 1000)} {
		t.Run(input, func(t *testing.T) {
			_, err := ParseCron(input)
			requireSafeCronError(t, err)
			requireSafeCronError(t, (Trigger{Kind: "cron", Cron: "@daily", Timezone: input}).Validate())
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
	batch, err := PlanRecurring(stored, definitionUID, p, anchor.Add(time.Hour))
	require.ErrorIs(t, err, ErrInvalid)
	require.Empty(t, batch.Due)
	require.Equal(t, p, batch.Progress)
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
