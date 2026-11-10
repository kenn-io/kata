package cron

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
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
	for _, b := range []byte(message) {
		require.True(t, b >= 0x20 && b != 0x7f, "%q", message)
	}
}

func TestCronValidationPropagatesDiagnostic(t *testing.T) {
	trigger := Trigger{Kind: "cron", Cron: "0 9 L * *"}
	adapter, err := ParseJob([]byte(executeJSON))
	require.NoError(t, err)
	for _, job := range []JobDefinition{adapter, nativeJob(t)} {
		job.Trigger = trigger
		require.ErrorContains(t, job.Validate(), "L, W, and # syntax is not supported")
	}
}

func TestCronLongErrorIsBounded(t *testing.T) {
	_, err := ParseCron(strings.Repeat("x", 1000) + " 9 * * *")
	requireSafeCronError(t, err)
}
