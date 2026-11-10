package cron

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestParseCronExplainsUnsupportedSyntax(t *testing.T) {
	for _, tc := range []struct{ expression, detail, forbidden string }{
		{"@reboot", "@reboot is not supported", ""}, {"@invented", "supported descriptors, such as @daily, must match exactly", ""},
		{"0 9 L * *", `day of month "L": last-day`, ""}, {"0 9 l * *", `day of month "l": last-day`, ""},
		{"0 9 15W * *", "nearest-weekday", ""}, {"0 9 15w * *", "nearest-weekday", ""},
		{"0 9 LW * *", `day of month "LW": last-weekday`, ""},
		{"0 9 * * FRI#L", `day of week "FRI#L": last-weekday`, ""},
		{"0 9 1,LW * *", `day of month "1,LW": last-weekday`, ""},
		{"0 9 * * MON,FRI#L", `day of week "MON,FRI#L": last-weekday`, ""},
		{"0 9 * * JUL", "numeric values or field-appropriate names", ""},
		{"0 9 * * 2#2", "nth-weekday", ""}, {"0 9 * * 5L", "last-weekday", ""},
		{"H 9 * * *", `minute "H": choose a fixed value`, ""},
		{"0 0 9 * * *", "only a 0 seconds field or wildcard * year", ""}, {"0 9 * * * 2026", "restricted year has no five-field equivalent", ""},
		{"0 0 9 * * * 2026", "7 fields", ""}, {"0 9 * *", "4 fields", ""}, {"5", "1 field;", ""},
		{"", "expression is empty", ""}, {"   ", "expression is empty", ""}, {" @daily", "descriptor must start", ""},
		{"60 9 * * *", `minute "60"`, ""}, {"0 9 * * 8", `day of week "8"`, ""},
		{"0 */24 * * *", `hour "*/24": step (24)`, ""}, {"*/0 * * * *", `minute "*/0"`, ""},
		{"0 9 * JAN-XYZ *", `month "JAN-XYZ"`, ""}, {"x\ny 9 * * *", "6 fields", ""},
		{"0 9 * * MON/7", "step (7)", "field-appropriate names"},
		{"0 9 * * MON-FRI/5", "step (5)", "field-appropriate names"},
		{"0 9 * * MON/0", "step of range", "field-appropriate names"},
		{"0 9 * * MON-FRI/0", "step of range", "field-appropriate names"},
		{"0 9 JUL * *", "day of month takes numbers 1-31", "option"},
		{"0 9 WED * *", "day of month takes numbers 1-31", "option"},
		{"@daily extra", "supported descriptors, such as @daily, must match exactly", "unknown descriptor"},
		{"@daily ", "supported descriptors, such as @daily, must match exactly", "unknown descriptor"},
		{"0 9 * * 5#", "nth-weekday", "option"},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			_, err := ParseCron(tc.expression)
			require.ErrorIs(t, err, ErrInvalid)
			require.Contains(t, err.Error(), "invalid cron document: invalid cron expression: ")
			require.Contains(t, err.Error(), tc.detail)
			if tc.forbidden != "" {
				require.NotContains(t, err.Error(), tc.forbidden)
			}
		})
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

func TestCronDirectiveFormatAdvice(t *testing.T) {
	for _, tc := range []struct{ expression, detail, forbidden string }{
		{" TZ=UTC 0 9 * * *", "one uppercase TZ= or CRON_TZ= directive", "seconds or year"},
		{"\tTZ=UTC @daily", "one uppercase TZ= or CRON_TZ= directive", "seconds or year"},
		{" CRON_TZ=UTC 0 9 * * *", "one uppercase TZ= or CRON_TZ= directive", "seconds or year"},
		{"tz=UTC 0 9 * * *", "one uppercase TZ= or CRON_TZ= directive", "seconds or year"},
		{"cron_tz=UTC 0 9 * * *", "one uppercase TZ= or CRON_TZ= directive", "seconds or year"},
		{"0 9 * * * TZ=UTC", "one uppercase TZ= or CRON_TZ= directive", "seconds or year"},
		{"TZ=UTC CRON_TZ=UTC 0 9 * * *", "one uppercase TZ= or CRON_TZ= directive", "seconds or year"},
		{"0 9 * TZ=UTC *", "one uppercase TZ= or CRON_TZ= directive", "seconds or year"},
		{"TZ=UTC\t0\t9\t*\t*\t*", "such as TZ=UTC 0 9 * * *; missing fields after timezone", "invalid timezone"},
		{"TZ=UTC", "such as TZ=UTC 0 9 * * *; missing fields after timezone", "invalid timezone"},
		{"TZ=UTC ", "such as TZ=UTC 0 9 * * *; missing fields after timezone", "invalid timezone"},
		{"CRON_TZ=UTC", "such as TZ=UTC 0 9 * * *; missing fields after timezone", "invalid timezone"},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			_, err := ParseCron(tc.expression)
			requireSafeCronError(t, err)
			require.Contains(t, err.Error(), tc.detail)
			require.NotContains(t, err.Error(), tc.forbidden)
		})
	}
	_, err := ParseCron("OTHER=UTC 0 9 * * *")
	require.ErrorContains(t, err, "6 fields")
}

func TestCronTimezoneAdvice(t *testing.T) {
	for _, tc := range []struct{ zone, expression, label, shown string }{
		{"Not/AZone", "@daily", "unknown time zone", "Not/AZone"},
		{"", "CRON_TZ=Not/AZone 0 9 * * *", "unknown time zone", "Not/AZone"},
		{"", `TZ="Not/AZone" 0 9 * * *`, "unknown time zone", "Not/AZone"},
		{"", "TZ=Not/AZone ", "unknown time zone", "Not/AZone"},
		{"", "TZ= 0 9 * * *", "invalid timezone", ""},
		{"", "TZ=Bad\tZone @daily", "invalid timezone", "Bad\tZone"},
		{"", "CRON_TZ=Bad\u00a0Zone 0 9 * * *", "invalid timezone", "Bad\u00a0Zone"},
		{"", "TZ=UTC\t0 9 * * *", "invalid timezone", "UTC\t0"},
		{"", "TZ=UTC\t 0 9 * * *", "invalid timezone", "UTC\t"},
		{"", "CRON_TZ=UTC\n0 9 * * *", "invalid timezone", "UTC\n0"},
		{"", `TZ="America/New York" 0 9 * * *`, "invalid timezone", `"America/New`},
		{"", "TZ=America/New York 0 9 * * *", "unknown time zone", "America/New"},
		{"../UTC", "@daily", "invalid timezone", "../UTC"},
		{"/abs", "@daily", "invalid timezone", "/abs"},
		{"bad zone!", "@daily", "invalid timezone", "bad zone!"},
		{"bad!", "@daily", "invalid timezone", "bad!"},
		{"bad!" + strings.Repeat("x", 1100), "@daily", "invalid timezone", "bad!" + strings.Repeat("x", 33) + "…"},
		{"A\x00B", "@daily", "invalid timezone", "A\x00B"},
		{" ", "@daily", "invalid timezone", " "},
		{`"UTC"`, "@daily", "invalid timezone", `"UTC"`},
		{"'UTC'", "@daily", "invalid timezone", "'UTC'"},
		{"x\x00y 9 * * *", "@daily", "invalid timezone", "x\x00y 9 * * *"},
		{"\x1b[31m 9 * * *", "@daily", "invalid timezone", "\x1b[31m 9 * * *"},
		{"x\x7fy 9 * * *", "@daily", "invalid timezone", "x\x7fy 9 * * *"},
		{string([]byte{0xff}) + " 9 * * *", "@daily", "invalid timezone", string([]byte{0xff}) + " 9 * * *"},
		{"bad\x00zone", "@daily", "invalid timezone", "bad\x00zone"},
		{strings.Repeat("\x00", 1000), "@daily", "invalid timezone", strings.Repeat("\x00", 37) + "…"},
		{strings.Repeat("x", 1000) + " 9 * * *", "@daily", "invalid timezone", strings.Repeat("x", 37) + "…"},
		{strings.Repeat("é", 1000), "@daily", "invalid timezone", strings.Repeat("é", 18) + "…"},
	} {
		t.Run(tc.zone+tc.expression, func(t *testing.T) {
			err := (Trigger{Kind: "cron", Cron: tc.expression, Timezone: tc.zone}).Validate()
			requireSafeCronError(t, err)
			want := fmt.Sprintf("invalid cron document: %s %q; use an IANA name such as America/New_York, or UTC", tc.label, tc.shown)
			if tc.zone == "" {
				want += "; a TZ= or CRON_TZ= zone name ends at the first ASCII space, and a schedule must follow it, such as TZ=UTC 0 9 * * *"
			}
			require.EqualError(t, err, want)
		})
	}
}

// Pin JobDefinition.Validate → Trigger.Validate; schedule-return paths are covered by the length test.
func TestCronValidationPropagatesDiagnostic(t *testing.T) {
	trigger := Trigger{Kind: "cron", Cron: "0 9 L * *"}
	_, err := ParseCron(trigger.Cron)
	adapter, decodeErr := ParseJob([]byte(executeJSON))
	require.NoError(t, decodeErr)
	native := nativeJob(t)
	for _, job := range []JobDefinition{adapter, native} {
		job.Trigger = trigger
		require.EqualError(t, job.Validate(), err.Error())
	}
}

func TestCronScheduleLengthIncludesTimezonePrefix(t *testing.T) {
	expression := "0" + strings.Repeat(" ", 1011) + "9 * * *"
	trigger := Trigger{Kind: "cron", Cron: expression}
	require.NoError(t, trigger.Validate())
	_, want := ParseCron("CRON_TZ=UTC " + expression)
	require.ErrorContains(t, want, "complete parser input")
	require.ErrorContains(t, want, "1031 bytes")
	require.ErrorContains(t, want, "1024 bytes")
	_, err := recurringNext(trigger, time.Time{})
	require.EqualError(t, err, want.Error())
	trigger.Timezone = "UTC"
	_, err = adapterCronSchedule(trigger)
	require.EqualError(t, err, want.Error())
}

func TestCronEveryDurationDiagnostics(t *testing.T) {
	for _, tc := range []struct{ expression, detail string }{
		{"@every", `unrecognized descriptor: "@every"`},
		{"@every 2x", `failed to parse duration "@every 2x"`},
		{"@every 1ms", "@every duration must be at least 1s"},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			_, err := ParseCron(tc.expression)
			require.ErrorContains(t, err, tc.detail)
			require.ErrorContains(t, err, "native recurring jobs use an interval trigger instead")
		})
	}
}
