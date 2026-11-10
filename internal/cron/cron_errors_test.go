package cron

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestParseCronExplainsUnsupportedSyntax(t *testing.T) {
	for _, tc := range []struct{ expression, detail string }{
		{"@reboot", "@reboot is not supported"}, {"@invented", `unknown descriptor "@invented"`},
		{"@every", "native recurring jobs use an interval trigger"}, {"@every 2x", "native recurring jobs use an interval trigger"},
		{"@every 1ms", "at least 1s"},
		{"0 9 L * *", `day of month "L": last-day`}, {"0 9 l * *", `day of month "l": last-day`},
		{"0 9 15W * *", "nearest-weekday"}, {"0 9 15w * *", "nearest-weekday"},
		{"0 9 * * 2#2", "nth-weekday"}, {"0 9 * * 5L", "last-weekday"},
		{"H 9 * * *", `minute "H": choose a fixed value`},
		{"0 0 9 * * *", "only a 0 seconds field or wildcard * year"}, {"0 9 * * * 2026", "restricted year has no five-field equivalent"},
		{"0 0 9 * * * 2026", "7 fields"}, {"0 9 * *", "4 fields"}, {"5", "1 field;"},
		{"", "expression is empty"}, {"   ", "expression is empty"}, {" @daily", "descriptor must start"},
		{"60 9 * * *", `minute "60"`}, {"0 9 * * 8", `day of week "8"`},
		{"0 */24 * * *", `hour "*/24": step (24)`}, {"*/0 * * * *", `minute "*/0"`},
		{"0 9 * JAN-XYZ *", `month "JAN-XYZ"`}, {"x\ny 9 * * *", "6 fields"},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			_, err := ParseCron(tc.expression)
			require.ErrorIs(t, err, ErrInvalid)
			require.Contains(t, err.Error(), "invalid cron document: invalid cron expression: ")
			require.Contains(t, err.Error(), tc.detail)
			adapter, parseErr := ParseJob([]byte(executeJSON))
			require.NoError(t, parseErr)
			adapter.Trigger = Trigger{Kind: "cron", Cron: tc.expression}
			native := nativeJob(t)
			native.Trigger = adapter.Trigger
			require.EqualError(t, adapter.Validate(), err.Error())
			require.EqualError(t, native.Validate(), err.Error())
		})
	}
}

func TestTriggerUnknownTimezoneNamesZone(t *testing.T) {
	want := `invalid cron document: unknown time zone "Not/AZone"; use an IANA name such as America/New_York, or UTC`
	for _, trigger := range []Trigger{
		{Kind: "cron", Cron: "0 9 * * *", Timezone: "Not/AZone"},
		{Kind: "cron", Cron: "CRON_TZ=Not/AZone 0 9 * * *"},
		{Kind: "cron", Cron: `TZ="Not/AZone" 0 9 * * *`},
	} {
		require.EqualError(t, trigger.Validate(), want)
	}
	for _, zone := range []string{strings.Repeat("é", 1000), strings.Repeat("\x00", 1000), "bad\x00zone", string([]byte{0xff})} {
		requireSafeCronError(t, (Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: zone}).Validate())
	}
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

func TestParseCronPreservesAcceptedGrammar(t *testing.T) {
	for _, expression := range []string{"70/5 * * * *", "70-5 * * * *", "0 22-2 * * *", "0 9 * * 5-1", "0 9 ? * 7", "0 9 * * 5-7/2", `TZ="UTC" @daily`, "@triggered", "@manual", "@none"} {
		_, err := ParseCron(expression)
		require.NoError(t, err, "%q", expression)
	}
	adapter, err := ParseJob([]byte(executeJSON))
	require.NoError(t, err)
	adapter.Trigger = Trigger{Kind: "cron", Cron: "@every 90m"}
	require.NoError(t, adapter.Validate())
	native := nativeJob(t)
	native.Trigger = adapter.Trigger
	require.EqualError(t, native.Validate(), "invalid cron document: use an interval trigger instead of @every")
}
