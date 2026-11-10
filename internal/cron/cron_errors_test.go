package cron

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/textsafe"
)

func TestParseCronExplainsUnsupportedSyntax(t *testing.T) {
	for _, tc := range []struct{ expression, hint string }{
		{"0 0 9 * * *", "Kata uses five fields"},
		{"0 0 9 * * * 2026", "Kata uses five fields"},
		{"@reboot", "@reboot is not supported"},
		{"@every 1ms", "use an interval trigger"},
		{"0 9 L JAN *", "L, W, and # syntax is not supported"},
		{"0 9 15W * *", "L, W, and # syntax is not supported"},
		{"0 9 * * 5#2", "L, W, and # syntax is not supported"},
		{"H 9 * * *", "hashed schedules (H) are not supported"},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			_, err := ParseCron(tc.expression)
			require.ErrorIs(t, err, ErrInvalid)
			require.ErrorContains(t, err, tc.hint)
		})
	}
	_, err := ParseCron("@every 1ms")
	require.NotContains(t, err.Error(), "needs a duration")
	_, err = ParseCron("0 9 L JAN *")
	require.NotContains(t, err.Error(), "no five-field equivalent")
	_, err = ParseCron("61 * * * *")
	require.ErrorContains(t, err, "above maximum")
}

func requireSafeCronError(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, ErrInvalid)
	message := err.Error()
	require.Less(t, len(message), 400)
	require.True(t, utf8.ValidString(message), "%q", message)
	for _, r := range message {
		require.False(t, r < 0x20 || textsafe.IsUnsafeTerminalRune(r), "%q", message)
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
	requireSafeCronError(t, err)
	require.ErrorContains(t, err, `\u202e`)
}

func TestCronLongErrorIsBounded(t *testing.T) {
	_, err := ParseCron(strings.Repeat("x", 1000) + " 9 * * *")
	requireSafeCronError(t, err)
}
