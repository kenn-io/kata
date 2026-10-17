package cron

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	robcron "github.com/robfig/cron/v3"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func requireSameCronOccurrences(t *testing.T, got, want robcron.Schedule, start time.Time) {
	t.Helper()
	for range 30 {
		gotNext, wantNext := got.Next(start), want.Next(start)
		require.False(t, wantNext.IsZero())
		require.True(t, gotNext.Equal(wantNext), "after %s: got %s, want %s", start, gotNext, wantNext)
		start = wantNext
	}
}

func TestParseCronAcceptsSevenAsSunday(t *testing.T) {
	for _, pair := range [][2]string{
		{"0 9 * * 7", "0 9 * * 0"},
		{"0 9 * * 1,7", "0 9 * * 0,1"},
		{"0 9 * * 5-7", "0 9 * * 0,5,6"},
		{"0 9 * * 6-7", "0 9 * * 0,6"},
		{"0 9 * * 0-7", "0 9 * * 0-6"},
		{"0 9 * * 0-7/1", "0 9 * * 0-6"},
		{"0 9 * * 1-7", "0 9 * * 0-6"},
		{"0 9 * * 5-7/2", "0 9 * * 0,5"},
		{"0 9 * * 7/2", "0 9 * * 0"},
		{"0 9 * * mon-7", "0 9 * * 0-6"},
		{"CRON_TZ=UTC 0 9 * * 7", "CRON_TZ=UTC 0 9 * * 0"},
		{"TZ=America/New_York 0 9 * * 7", "TZ=America/New_York 0 9 * * 0"},
		{"TZ=UTC 0\t9 * * 7", "TZ=UTC 0\t9 * * 0"},
		{"0 9 * * 7/9223372036854775807", "0 9 * * 0"},
		{"0 9 * * 07", "0 9 * * 0"},
		{"0 9 * * +7", "0 9 * * 0"},
		{"0 9 * * +007", "0 9 * * 0"},
		{"0 9 * * 7/+2", "0 9 * * 0"},
		{"0 9 * * SuN-7/2", "0 9 * * 0,2,4,6"},
		{"0 9 * * sAt-007", "0 9 * * 0,6"},
		{"0 9 * * 5/2,7", "0 9 * * 0,5"},
		{"0 9 * * 0-7/9223372036854775807", "0 9 * * 0"},
		{"0 9 * * 6-7/9223372036854775807", "0 9 * * 6"},
		{"0 9 * * SAT-7", "0 9 * * 0,6"},
		{"0 9 * * 7,0,7", "0 9 * * 0"},
		{"0 9 * * ,7", "0 9 * * ,0"},
		{"0 9 * * 7,", "0 9 * * 0,"},
	} {
		t.Run(pair[0], func(t *testing.T) {
			got, err := ParseCron(pair[0])
			require.NoError(t, err)
			want, err := ParseCron(pair[1])
			require.NoError(t, err)
			requireSameCronOccurrences(t, got, want, at(t, "2026-10-17T00:00:00Z"))
		})
	}
}

func TestParseCronSevenKeepsStarBit(t *testing.T) {
	for _, tc := range []struct {
		seven, zero string
		dow         uint64
	}{
		{"0 9 15 * 0-7", "0 9 15 * 0-6", 0x7f},
		{"0 9 15 * 7,*", "0 9 15 * 0,*", 1<<63 | 0x7f},
		{"0 9 15 * */2,7", "0 9 15 * */2,0", 0x55},
		{"0 9 15 * *,7", "0 9 15 * *,0", 1<<63 | 0x7f},
		{"0 9 15 * ?/2,7", "0 9 15 * ?/2,0", 0x55},
	} {
		t.Run(tc.seven, func(t *testing.T) {
			got, err := ParseCron(tc.seven)
			require.NoError(t, err)
			want, err := ParseCron(tc.zero)
			require.NoError(t, err)
			require.Equal(t, tc.dow, got.(*robcron.SpecSchedule).Dow)
			require.Equal(t, want.(*robcron.SpecSchedule).Dow, got.(*robcron.SpecSchedule).Dow)
			requireSameCronOccurrences(t, got, want, at(t, "2026-10-17T00:00:00Z"))
		})
	}
}

func TestNativeRecurringSundaySeven(t *testing.T) {
	job := nativeJob(t)
	job.Trigger = Trigger{Kind: "cron", Cron: "0 9 * * 7", Timezone: "America/New_York"}
	job.Catchup = "all"
	require.NoError(t, job.Validate())
	anchor := at(t, "2026-10-17T00:00:00Z")
	batch, err := PlanRecurring(job, definitionUID, RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}, at(t, "2026-10-19T00:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, []time.Time{at(t, "2026-10-18T13:00:00Z")}, batch.Due)
}

func TestCronJobKeepsSevenAsWritten(t *testing.T) {
	document := strings.Replace(executeJSON, `"kind":"manual"`, `"kind":"cron","cron":"0 9 * * 5-7"`, 1)
	job, err := ParseJob([]byte(document))
	require.NoError(t, err)
	raw, err := json.Marshal(job)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"cron":"0 9 * * 5-7"`)
}

func TestParseCronDayOfWeekStillRejected(t *testing.T) {
	for _, expression := range []string{
		"0 9 * * 8", "0 9 * * 7-8", "0 9 * * 7-1", "0 9 * * MON-SUN",
		"0 9 * * 1-7/0", "0 9 * * 7/-1", "0 9 * * 1-7/-1",
		"0 9 * * 7/99999999999999999999", "0 9 * * 7 *", "TZ=UTC\t0 9 * * 7",
		"0 9 * * 7,8", "0 9 * * 7-0", "0 9 * * 8/2", "0 9 * * 7/0", "0 9 * * 7//2",
		"0 9 * * 99999999999999999999",
		"TZ=UTC\t * * * * 7", "CRON_TZ=UTC\n * * * * 7",
		"0 9 * * 7/2/3", "0 9 * * 1-7-7", "0 9 * * unknown-7", "0 9 * * 7-L",
	} {
		t.Run(expression, func(t *testing.T) {
			_, err := ParseCron(expression)
			require.Error(t, err)
		})
	}
}

func TestParseCronStoredExpressionsUnchanged(t *testing.T) {
	parser := robcron.NewParser(robcron.Minute | robcron.Hour | robcron.Dom | robcron.Month | robcron.Dow | robcron.Descriptor)
	for _, expression := range []string{
		"0 9 * JAN MON-FRI", "@hourly", "@daily", "@weekly", "@monthly", "@yearly", "@every 90m", "@every 2h",
		"TZ=UTC @every 2h", "CRON_TZ=UTC @every 2h", "CRON_TZ=America/New_York 0 9 * * *",
		"0 0 1 * *", "0 9 * * 1-5", "* * * * *", "30 2 * * *", "30 1 * * *", "0 9 * * *",
		"7 7 7 7 *", "*/7 7 * * 1-5", "0 9 * * */7", "0 9 1 * */2", "0 9 1 * 1,*",
		"0 9 * * 6/2", "0 9 * * 5/2", "0 9 * * 6/9223372036854775807", "0 9 * * MON-FRI", "0 9 1-7 * 2",
		"TZ=UTC 0\t9 * * 0", "CRON_TZ=UTC 0 9 * * 0", "0 9 * * ?", "0 9 * * ?/2", "0 9 * * 1,?", "0 9 * * ,0", "0 9 * * 0,",
	} {
		for _, zone := range []string{"UTC", "America/New_York"} {
			t.Run(zone+"/"+expression, func(t *testing.T) {
				location, err := time.LoadLocation(zone)
				require.NoError(t, err)
				unchanged, err := normalizeDayOfWeek(expression)
				require.NoError(t, err)
				require.Equal(t, expression, unchanged, "previously accepted expressions must reach robfig verbatim")
				got, err := ParseCron(expression)
				require.NoError(t, err)
				want, err := parser.Parse(expression)
				require.NoError(t, err)
				if spec, ok := want.(*robcron.SpecSchedule); ok {
					require.Equal(t, spec.Dow, got.(*robcron.SpecSchedule).Dow)
				}
				requireSameCronOccurrences(t, got, want, at(t, "2026-10-17T00:00:00Z").In(location))
			})
		}
	}
}

// All valid ranges ending on the alternate Sunday must denote the same
// weekday set as enumerating the numeric range and folding 7 onto Sunday.
func TestParseCronSundaySevenRangeProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		start := rapid.IntRange(0, 7).Draw(t, "start")
		step := rapid.IntRange(1, math.MaxInt).Draw(t, "step")
		schedule, err := ParseCron(fmt.Sprintf("0 9 * * %d-7/%d", start, step))
		require.NoError(t, err)
		var want uint64
		for day := start; day <= 7; {
			want |= 1 << (day % 7)
			if step > 7-day {
				break
			}
			day += step
		}
		require.Equal(t, want, schedule.(*robcron.SpecSchedule).Dow)
	})
}

func TestNormalizeDayOfWeekDiagnostics(t *testing.T) {
	for _, tc := range []struct{ field, message string }{
		{"8", "above maximum (7)"},
		{"7-8", "above maximum (7)"},
		{"1-7/0", "step of range should be a positive number"},
		{"7/-1", "negative number (-1) not allowed"},
		{"1-7/-1", "negative number (-1) not allowed"},
		{"7/99999999999999999999", "failed to parse int from"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			_, err := normalizeDayOfWeek("0 9 * * " + tc.field)
			require.ErrorContains(t, err, tc.message)
		})
	}
}

// A new alias in a list must not extend legacy N/step elements through 7.
func TestParseCronSundaySevenListProperty(t *testing.T) {
	parser := robcron.NewParser(robcron.Minute | robcron.Hour | robcron.Dom | robcron.Month | robcron.Dow)
	rapid.Check(t, func(t *rapid.T) {
		start := rapid.IntRange(0, 6).Draw(t, "start")
		step := rapid.IntRange(1, math.MaxInt).Draw(t, "step")
		got, err := ParseCron(fmt.Sprintf("0 9 15 * %d/%d,7", start, step))
		require.NoError(t, err)
		want, err := parser.Parse(fmt.Sprintf("0 9 15 * %d/%d,0", start, step))
		require.NoError(t, err)
		require.Equal(t, want.(*robcron.SpecSchedule).Dow, got.(*robcron.SpecSchedule).Dow)
	})
}

func TestParseCronDayOfWeekOutOfBoundsProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		day := rapid.IntRange(8, math.MaxInt).Draw(t, "day")
		_, err := ParseCron(fmt.Sprintf("0 9 * * 7,%d", day))
		require.Error(t, err)
	})
}
