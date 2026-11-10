package cron

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseCronExplainsUnsupportedSyntax(t *testing.T) {
	for _, tc := range []struct{ expression, hint string }{
		{"0 0 9 * * *", "Kata uses five fields"},
		{"0 0 9 * * * 2026", "Kata uses five fields"},
		{"@reboot", "@reboot is not supported"},
		{"@every 1ms", "adapter jobs can use @every 1s or longer; native recurring jobs use an interval trigger"},
		{"0 9 L JAN *", "L, W, and # syntax is not supported"},
		{"0 9 15W * *", "L, W, and # syntax is not supported"},
		{"0 9 * * 5#2", "L, W, and # syntax is not supported"},
		{"0 9 * * L", "L, W, and # syntax is not supported"},
		{"H 9 * * *", "hashed schedules (H) are not supported"},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			_, err := ParseCron(tc.expression)
			require.ErrorIs(t, err, ErrInvalid)
			require.ErrorContains(t, err, tc.hint)
		})
	}
	for _, expression := range []string{"@daily 1 2 3 4 5", "TZ=UTC @daily 1 2 3 4 5", "@daily 1 L JAN *"} {
		t.Run(expression, func(t *testing.T) {
			_, err := ParseCron(expression)
			require.ErrorContains(t, err, "unrecognized descriptor")
			require.NotContains(t, err.Error(), "Kata uses five fields")
			require.NotContains(t, err.Error(), "L, W, and # syntax is not supported")
		})
	}
}

func TestCronValidationPropagatesDiagnostic(t *testing.T) {
	expression := "0" + strings.Repeat(" ", 1011) + "9 * * *"
	trigger := Trigger{Kind: "cron", Cron: expression, Timezone: "UTC"}
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	progress := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}
	native := nativeJob(t)
	native.Trigger = trigger
	_, err := PlanRecurring(native, definitionUID, progress, anchor.Add(time.Hour))
	require.ErrorContains(t, err, "spec too long: 1031 > 1024")
	adapter := adapterJob(t, trigger)
	_, err = RecurringOccurrences(adapter, progress, anchor, 1)
	require.ErrorContains(t, err, "spec too long: 1031 > 1024")
}

func TestCronErrorEscapesDirectionControl(t *testing.T) {
	_, err := ParseCron("\u202e0 9 * *")
	require.ErrorIs(t, err, ErrInvalid)
	require.NotContains(t, err.Error(), "\u202e")
	require.ErrorContains(t, err, `\u202e`)
}
