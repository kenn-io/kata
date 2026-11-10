package cron

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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

// Every actual rejection has one safe diagnostic, irrespective of accepted grammar.
func FuzzCronErrorSafety(f *testing.F) {
	for _, input := range []string{
		"TZ=UTC", "CRON_TZ=UTC", "TZ=UTC\t0 9 * * *", "TZ=UTC @daily",
		"x\x00y 9 * * *", "\x1b[31m 9 * * *", "x\x7fy 9 * * *", string([]byte{0xff}) + " 9 * * *",
		"TZ=" + strings.Repeat("\x00", 40) + " @daily", "TZ=" + strings.Repeat("\xff", 40) + " @daily",
		"bad\x00zone", strings.Repeat("\x00", 1000), strings.Repeat("x", 1000) + " 9 * * *", strings.Repeat("é", 1000),
		"@invented", "@daily ", "@every 2x", " @daily", "0 9 * * 5#", "0 9 L-3 * *", "0 9 15W * *", "H * * * *",
		"60 * * * *", "* */24 * * *", "0 9 * JAN-XYZ *", "0 9 * * MON/0", "0 9 * * MON,,FRI", "0 9 * * -1",
		`TZ="bad!" @daily`, "CRON_TZ=bad! @daily", `TZ="UTC @daily`, "TZ=UTC\u00a0@daily", "TZ=UTC\t @daily",
		"CRON_TZ=UTC\n@daily", "TZ=UTC TZ=UTC @daily", "0 9 * * * TZ=UTC", "../UTC", "/abs", `"UTC"`, "'UTC'",
	} {
		f.Add(input)
	}
	for _, b := range []byte("\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f") {
		f.Add("x" + string(b) + "y 9 * * *")
		f.Add(strings.Repeat(string(b), 1000))
	}
	for _, value := range []string{"\xff", "\xc3", "\xc0\xaf", "\xed\xa0\x80", `"`, `\`, "\x00", "é", "界"} {
		for _, n := range []int{37, 40, 327, 330, 1023, 1024, 1025} {
			f.Add(strings.Repeat(value, n) + " 9 * * *")
			f.Add("TZ=" + strings.Repeat("x", n) + value + " @daily")
		}
	}
	f.Fuzz(func(t *testing.T, input string) {
		if _, err := ParseCron(input); err != nil {
			requireSafeCronError(t, err)
		}
		if err := (Trigger{Kind: "cron", Cron: "@daily", Timezone: input}).Validate(); err != nil {
			requireSafeCronError(t, err)
		}
	})
}
