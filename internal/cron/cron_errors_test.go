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
		{"0 9 LW * *", `day of month "LW": last-weekday`},
		{"0 9 * * FRI#L", `day of week "FRI#L": last-weekday`},
		{"0 9 1,LW * *", `day of month "1,LW": last-weekday`},
		{"0 9 * * MON,FRI#L", `day of week "MON,FRI#L": last-weekday`},
		{"0 9 WED * *", "numeric values or field-appropriate names"},
		{"0 9 JUL * *", "numeric values or field-appropriate names"},
		{"0 9 * * JUL", "numeric values or field-appropriate names"},
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
		{Kind: "cron", Cron: "TZ=Not/AZone "},
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
	for _, expression := range []string{"70/5 * * * *", "70-5 * * * *", "0 22-2 * * *", "0 9 * * 5-1", "0 9 ? * 7", `TZ="UTC" @daily`, "@triggered", "@manual", "@none"} {
		_, err := ParseCron(expression)
		require.NoError(t, err, "%q", expression)
	}
	native := nativeJob(t)
	native.Trigger = Trigger{Kind: "cron", Cron: "@every 90m"}
	require.EqualError(t, native.Validate(), "invalid cron document: use an interval trigger instead of @every")
}

func TestCronDirectiveFormatAdvice(t *testing.T) {
	for _, expression := range []string{
		" TZ=UTC 0 9 * * *", " CRON_TZ=UTC 0 9 * * *", "tz=UTC 0 9 * * *", "cron_tz=UTC 0 9 * * *",
		"0 9 * * * TZ=UTC", "TZ=UTC CRON_TZ=UTC 0 9 * * *", "0 9 * TZ=UTC *",
	} {
		t.Run(expression, func(t *testing.T) {
			_, err := ParseCron(expression)
			requireSafeCronError(t, err)
			require.Contains(t, err.Error(), "one uppercase TZ= or CRON_TZ= directive")
			require.NotContains(t, err.Error(), "seconds or year")
		})
	}
	_, err := ParseCron("OTHER=UTC 0 9 * * *")
	require.ErrorContains(t, err, "6 fields")
}

func TestTriggerTimezoneSyntaxAdvice(t *testing.T) {
	for _, zone := range []string{"../UTC", "/abs", "bad zone!", "bad!", "A\x00B", " ", `"UTC"`, "'UTC'"} {
		t.Run(zone, func(t *testing.T) {
			err := (Trigger{Kind: "cron", Cron: "@daily", Timezone: zone}).Validate()
			requireSafeCronError(t, err)
			require.Contains(t, err.Error(), "invalid timezone")
			require.NotContains(t, err.Error(), "unknown time zone")
		})
	}
	for _, zone := range []string{"", "UTC", "Local"} {
		require.NoError(t, (Trigger{Kind: "cron", Cron: "@daily", Timezone: zone}).Validate())
	}
}
