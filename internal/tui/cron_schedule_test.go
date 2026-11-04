package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

// The reviewed contract names a deliberately narrow grammar; everything else
// must keep its raw text, even when the shared parser accepts it.
func TestCronScheduleLabels(t *testing.T) {
	for _, tc := range []struct{ expr, want string }{
		{"0 * * * *", "Hourly"}, {"59 * * * *", "Hourly at :59"},
		{"30 8 * * *", "Daily 08:30"}, {"0 9 * * 1-5", "Weekdays 09:00"},
		{"0 0 * * 0", "Sundays 00:00"}, {"0 0 * * 7", "Sundays 00:00"},
		{"0 9 * * 1", "Mondays 09:00"}, {"0 9 * * 2", "Tuesdays 09:00"},
		{"0 9 * * 3", "Wednesdays 09:00"}, {"0 9 * * 4", "Thursdays 09:00"},
		{"0 17 * * 5", "Fridays 17:00"}, {"0 9 * * 6", "Saturdays 09:00"},
		{"00 09 * * *", "Daily 09:00"}, {"  0\t9 * * 1\t", "Mondays 09:00"},
		{"@hourly", "Hourly"}, {"@daily", "Daily 00:00"},
		{"@midnight", "Daily 00:00"}, {"@weekly", "Sundays 00:00"},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			require.Equal(t, tc.want, cronSchedule(generated.TriggerResponse{Kind: "cron", Cron: new(tc.expr)}))
		})
	}
	for _, expr := range []string{
		"", "invalid", "60 * * * *", "0 24 * * *", "0 9 * * 8",
		"+0 9 * * *", "0 +9 * * *", "0 9 * * 01", "0 9 * * MON",
		"*/7 * * * *", "0 */2 * * *", "0 9 * * 1,3", "0 9 * * 1-4",
		"0 9 1 * *", "0 9 * 1 *", "0 9 15 * 1", "0 9 * * 5-1",
		"0 * * * 1", "0 * * * 1-5", "0 9 * * 07", "0 9 * * -1",
		"999999999999999999999999 9 * * *", "0 999999999999999999999999 * * *",
		"@monthly", "@yearly", "@annually", "@every 1h",
		"TZ=UTC 0 9 * * *", "CRON_TZ=Europe/Paris 0 9 * * *",
		"0 9 * * *\x00", "0 9 * * * extra",
	} {
		t.Run(expr+" raw", func(t *testing.T) {
			require.Equal(t, expr, cronSchedule(generated.TriggerResponse{Kind: "cron", Cron: new(expr)}))
		})
	}
}

func TestCronScheduleWeekdaysAtMidnightFromProperty(t *testing.T) {
	require.Equal(t, "Weekdays 00:00", cronSchedule(generated.TriggerResponse{Kind: "cron", Cron: new("0 0 * * 1-5")}))
}

func TestCronScheduleZonesAndDetail(t *testing.T) {
	for _, zone := range []string{"", "UTC", "Europe/Paris"} {
		for _, tc := range []struct{ expr, list, detail string }{
			{"0 9 * * 1-5", "Weekdays 09:00", "Weekdays 09:00 · 0 9 * * 1-5"},
			{"*/7 * * * *", "*/7 * * * *", "*/7 * * * *"},
		} {
			suffix := ""
			if zone != "" && zone != "UTC" {
				suffix = " (" + zone + ")"
			}
			trigger := generated.TriggerResponse{Kind: "cron", Cron: new(tc.expr), Timezone: new(zone)}
			require.Equal(t, tc.list+suffix, cronSchedule(trigger))
			m := cronFixture(t)
			m.width = 160
			m.cron.data.jobs[0].Definition.Trigger = trigger
			m = sendKey(m, '\r')
			require.Contains(t, stripANSI(m.viewBody()), tc.detail+suffix)
		}
	}
}

func TestCronScheduleSupportedGrammar(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		minute := hegel.Draw(ht, hegel.Integers(0, 59))
		hour := hegel.Draw(ht, hegel.Integers(0, 23))
		day := hegel.Draw(ht, hegel.SampledFrom([]string{"*", "1-5", "0", "1", "2", "3", "4", "5", "6", "7"}))
		m, h := fmt.Sprint(minute), fmt.Sprint(hour)
		if hegel.Draw(ht, hegel.Booleans()) {
			m, h = "0"+m, "0"+h
		}
		want := fmt.Sprintf("Daily %02d:%02d", hour, minute)
		switch day {
		case "*":
			if hegel.Draw(ht, hegel.Booleans()) {
				h = "*"
				want = "Hourly"
				if minute != 0 {
					want = fmt.Sprintf("Hourly at :%02d", minute)
				}
			}
		case "1-5":
			want = fmt.Sprintf("Weekdays %02d:%02d", hour, minute)
		default:
			days := []string{"Sundays", "Mondays", "Tuesdays", "Wednesdays", "Thursdays", "Fridays", "Saturdays", "Sundays"}
			want = fmt.Sprintf("%s %02d:%02d", days[int(day[0]-'0')], hour, minute)
		}
		expr := fmt.Sprintf("%s %s * * %s", m, h, day)
		if hegel.Draw(ht, hegel.Booleans()) {
			expr = " \t" + strings.ReplaceAll(expr, " ", "\t ") + " "
		}
		got := cronSchedule(generated.TriggerResponse{Kind: "cron", Cron: new(expr)})
		require.Equal(ht, want, got)
		assertCronLabelOccurrences(ht, expr, got)
	}, hegel.WithTestCases(1000))
}

func TestCronScheduleArbitraryText(t *testing.T) {
	seeds := []string{"0 9 * * *", "0 9 * * 1-5", "@hourly", "@daily", "@midnight", "@weekly", "@monthly", "0 9 15 * 1"}
	mutated := hegel.FlatMap(hegel.SampledFrom(seeds), func(expr string) hegel.Generator[string] {
		return hegel.Map(hegel.Text(), func(extra string) string { return expr + extra })
	})
	hegel.Test(t, func(ht *hegel.T) {
		expr := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.SampledFrom(seeds), mutated))
		got := cronSchedule(generated.TriggerResponse{Kind: "cron", Cron: new(expr)})
		if got != expr {
			assertCronLabelOccurrences(ht, expr, got)
		}
	}, hegel.WithTestCases(1000))
}

// Interpret the displayed promise, independently of the cron recognizer. Exact
// next instants detect both missing fires and extra fires (e.g. Monday vs weekdays).
func assertCronLabelOccurrences(ht *hegel.T, expr, label string) {
	schedule, err := cron.ParseCron(expr)
	require.NoError(ht, err)
	fields := strings.Fields(label)
	hour, minute := 0, 0
	hourly := fields[0] == "Hourly"
	days := map[string]int{"Sundays": 0, "Mondays": 1, "Tuesdays": 2, "Wednesdays": 3, "Thursdays": 4, "Fridays": 5, "Saturdays": 6}
	if hourly {
		if len(fields) > 1 {
			_, err = fmt.Sscanf(label, "Hourly at :%d", &minute)
			require.NoError(ht, err)
		}
	} else {
		require.Len(ht, fields, 2)
		_, err = fmt.Sscanf(fields[1], "%d:%d", &hour, &minute)
		require.NoError(ht, err)
		_, weekday := days[fields[0]]
		require.True(ht, fields[0] == "Daily" || fields[0] == "Weekdays" || weekday)
	}
	at := time.Unix(hegel.Draw(ht, hegel.Integers[int64](946684800, 1924991999)), 0).UTC()
	for range 10 {
		var want time.Time
		if hourly {
			want = at.Truncate(time.Hour).Add(time.Duration(minute) * time.Minute)
			if !want.After(at) {
				want = want.Add(time.Hour)
			}
		} else {
			want = time.Date(at.Year(), at.Month(), at.Day(), hour, minute, 0, 0, time.UTC)
			for {
				weekday := int(want.Weekday())
				matches := fields[0] == "Daily" || fields[0] == "Weekdays" && weekday >= 1 && weekday <= 5
				if d, ok := days[fields[0]]; ok {
					matches = weekday == d
				}
				if want.After(at) && matches {
					break
				}
				want = want.AddDate(0, 0, 1)
			}
		}
		got := schedule.Next(at)
		require.Equal(ht, want, got, "%q labeled %q", expr, label)
		require.True(ht, got.After(at))
		at = got
	}
}
