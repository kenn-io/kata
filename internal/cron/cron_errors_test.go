package cron

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCronExplainsUnsupportedSyntax(t *testing.T) {
	for _, tc := range []struct{ expression, want string }{
		{"@reboot", "@reboot is not supported"},
		{"@every 2x", `@every needs a duration such as 90m or 2h, got "2x"`},
		{"@every", `@every needs a duration such as 90m or 2h, got ""`},
		{"@invented", `unknown descriptor "@invented"`},
		{"0 9 L * *", `day of month "L": last-day syntax (L) is not supported`},
		{"0 9 15W * *", `day of month "15W": nearest-weekday syntax (W) is not supported`},
		{"0 9 * * 2#2", `day of week "2#2": nth-weekday syntax (#) is not supported`},
		{"0 9 * * 5L", `day of week "5L": last-weekday syntax (L) is not supported`},
		{"H 9 * * *", `minute "H": hashed values (H) are not supported; choose a fixed value`},
		{"H(0-20) 9 * * *", `minute "H(0-20)": hashed values (H)`},
		{"H/15 9 * * *", `minute "H/15": hashed values (H)`},
		{"0 0 9 * * *", "6 fields; Kata uses exactly five"},
		{"30 0 9 * * *", "nonzero seconds or a year restriction have no five-field equivalent"},
		{"0 9 * * * *", "Removing an extra field keeps the schedule only if it was 0 seconds or a * year"},
		{"0 9 * * * 2026", "6 fields; Kata uses exactly five"},
		{"0 0 9 * * * 2026", "7 fields; Kata uses exactly five"},
		{"0 9 * *", "4 fields; Kata uses five"},
		{"5", "1 field; Kata uses five"},
		{"60 9 * * *", `minute "60": 60 is outside 0-59`},
		{"0 24 * * *", `hour "24": 24 is outside 0-23`},
		{"0 0 0 * *", `day of month "0": 0 is outside 1-31`},
		{"0 9 31-0 * *", `day of month "31-0": 0 is outside 1-31`},
		{"0 9 * 12-0 *", `month "12-0": 0 is outside 1-12`},
		{"0 9 * 13 *", `month "13": 13 is outside 1-12 (names JAN-DEC also work)`},
		{"0 9 * * 7", `day of week "7": 7 is above the maximum 6; use 0 or SUN for Sunday`},
		{"0 9 * * 8", `day of week "8": 8 is outside 0-6 (0 is Sunday)`},
		{"0 9 * * 5-1", `day of week "5-1": range start is after its end; ranges do not wrap, use 5-6,0-1`},
		{"0 22-2 * * *", `hour "22-2": range start is after its end; ranges do not wrap, use 22-23,0-2`},
		{"0 9 * * 6-1", `day of week "6-1": range start is after its end; ranges do not wrap, use 6,0-1`},
		{"0 9 * * 5-1/2", `day of week "5-1/2": range start is after its end; ranges do not wrap`},
		{"70/5 * * * *", `minute "70/5": 70 is outside 0-59`},
		{"*/0 * * * *", `minute "*/0": steps must be one positive number, as in */15`},
		{"*/2/3 * * * *", `minute "*/2/3": steps must be one positive number, as in */15`},
		{"0 9 * JAN-XYZ *", `month "JAN-XYZ": not a number, name, range, list or step`},
		{"", "empty; use five fields"},
		{"   ", "empty; use five fields"},
		{" TZ=UTC 0 9 * * *", "timezone directive must start the expression; remove leading whitespace"},
		{"\tCRON_TZ=UTC @daily", "timezone directive must start the expression; remove leading whitespace"},
		{"TZ=UTC TZ=UTC 0 9 * * *", "use only one TZ= or CRON_TZ= timezone directive"},
		{"CRON_TZ=UTC TZ=UTC 0 9 * * *", "use only one TZ= or CRON_TZ= timezone directive"},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			_, err := ParseCron(tc.expression)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalid)
			assert.Contains(t, err.Error(), "invalid cron expression: ")
			assert.Contains(t, err.Error(), tc.want)
			adapter, parseErr := ParseJob([]byte(executeJSON))
			require.NoError(t, parseErr)
			native := nativeJob(t)
			for _, job := range []*JobDefinition{&adapter, &native} {
				job.Trigger = Trigger{Kind: "cron", Cron: tc.expression}
				got := job.Validate()
				assert.ErrorIs(t, got, ErrInvalid)
				assert.EqualError(t, got, err.Error())
			}
		})
	}
	for _, suggestion := range []string{"0 9 * * 5-6,0-1", "0 22-23,0-2 * * *", "0 9 * * 6,0-1", "*/15 * * * *", "0 9 * * SUN", "@daily", "@every 90m", "0 9 * JAN-DEC *"} {
		_, err := ParseCron(suggestion)
		require.NoError(t, err, suggestion)
	}
	for _, valid := range []string{"0 9 * JUL WED", "TZ=UTC @daily"} {
		_, err := ParseCron(valid)
		require.NoError(t, err)
	}
	native := nativeJob(t)
	native.Trigger = Trigger{Kind: "cron", Cron: "@every 2h"}
	require.ErrorContains(t, native.Validate(), "use an interval trigger instead of @every")
}

func TestParseCronDirectiveWithoutScheduleIsExplained(t *testing.T) {
	for _, expression := range []string{"TZ=UTC", "CRON_TZ=UTC", "TZ=UTC\t0 9 * * *", "TZ=UTC\t0\t9\t*\t*\t*"} {
		t.Run(expression, func(t *testing.T) {
			_, parseErr := ParseCron(expression)
			require.Error(t, parseErr)
			assert.ErrorIs(t, parseErr, ErrInvalid)
			assert.ErrorContains(t, parseErr, "invalid cron expression: TZ= and CRON_TZ= must be followed by a space and a schedule")

			native := nativeJob(t)
			native.Trigger = Trigger{Kind: "cron", Cron: expression}
			for _, err := range []error{native.Trigger.Validate(), native.Validate()} {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrInvalid)
				assert.ErrorContains(t, err, "invalid cron expression: TZ= and CRON_TZ= must be followed by a space and a schedule")
			}
		})
	}
}

func TestTriggerUnknownTimezoneNamesZone(t *testing.T) {
	for _, trigger := range []Trigger{
		{Kind: "cron", Cron: "0 9 * * *", Timezone: "Not/AZone"},
		{Kind: "cron", Cron: "CRON_TZ=Not/AZone 0 9 * * *"},
	} {
		err := trigger.Validate()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalid)
		assert.EqualError(t, err, `invalid cron document: timezone "Not/AZone" is not a known IANA name; use one such as America/New_York, or UTC`)
	}
}

func TestParseCronErrorTextIsEscapedAndBounded(t *testing.T) {
	for _, token := range []string{"x\x00y", "\x1b[31m", strings.Repeat("x", 10_000), strings.Repeat("é", 5_000), string([]byte{0xff, 0xfe}), strings.Repeat("\x00", 10_000)} {
		_, err := ParseCron(token + " 9 * * *")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalid)
		text := err.Error()
		assert.Contains(t, text, "minute")
		assert.Less(t, len(text), 400)
		assert.True(t, utf8.ValidString(text))
		for _, b := range []byte(text) {
			if b < 0x20 || b == 0x7f {
				assert.Fail(t, "unescaped control in cron error")
				break
			}
		}
	}
	_, err := ParseCron("x\ny 9 * * *")
	require.ErrorContains(t, err, "6 fields; Kata uses exactly five")
}

func FuzzParseCronErrorText(f *testing.F) {
	for _, expression := range []string{"x\x00y 9 * * *", "0 9 * * 5-1", "@reboot", "TZ=UTC", "CRON_TZ=UTC", "TZ=UTC\t0 9 * * *", "TZ=UTC\t0\t9\t*\t*\t*", "TZ=UTC @daily", "0 9 * JUL WED", "CRON_TZ=Not/AZone @daily"} {
		f.Add(expression)
	}
	f.Fuzz(func(t *testing.T, expression string) {
		_, err := ParseCron(expression)
		if err == nil {
			return
		}
		text := err.Error()
		if !errors.Is(err, ErrInvalid) || len(text) >= 400 || !utf8.ValidString(text) {
			t.Fatalf("unbounded or unclassified error: %q", text)
		}
		for _, b := range []byte(text) {
			if b < 0x20 || b == 0x7f {
				t.Fatalf("unescaped control in error: %q", text)
			}
		}
	})
}
