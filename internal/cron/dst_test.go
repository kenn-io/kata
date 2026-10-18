package cron

import (
	"fmt"
	"testing"
	"time"

	robcron "github.com/robfig/cron/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func dstJob(t testing.TB, zone, expression string) JobDefinition {
	t.Helper()
	j := nativeJob(t)
	j.Trigger = Trigger{Kind: "cron", Cron: expression, Timezone: zone}
	return j
}

func dstDue(t testing.TB, zone, expression, start, end string, want ...string) {
	t.Helper()
	a := at(t, start)
	b, err := PlanRecurring(dstJob(t, zone, expression), definitionUID, RecurringProgress{Version: 1, Anchor: a, Through: a}, at(t, end))
	require.NoError(t, err)
	var expected []time.Time
	for _, s := range want {
		expected = append(expected, at(t, s))
	}
	require.Equal(t, expected, b.Due)
	require.False(t, b.More)
}

func TestRecurringDSTGapRunsAfterJump(t *testing.T) {
	dstDue(t, "America/New_York", "30 2 * * *", "2026-03-07T08:00:00Z", "2026-03-09T07:00:00Z", "2026-03-08T07:00:00Z", "2026-03-09T06:30:00Z")
}

func TestRecurringDSTGapCollapses(t *testing.T) {
	for _, expr := range []string{"0,30 2 * * *", "0 2,3 * * *"} {
		t.Run(expr, func(t *testing.T) {
			dstDue(t, "America/New_York", expr, "2026-03-08T05:00:00Z", "2026-03-08T08:00:00Z", "2026-03-08T07:00:00Z")
		})
	}
}

func TestRecurringDSTGapRespectsDayFields(t *testing.T) {
	dstDue(t, "America/New_York", "30 2 * * 0", "2026-03-07T08:00:00Z", "2026-03-09T07:00:00Z", "2026-03-08T07:00:00Z")
	dstDue(t, "America/New_York", "30 2 * * 1-5", "2026-03-06T00:00:00Z", "2026-03-10T00:00:00Z", "2026-03-06T07:30:00Z", "2026-03-09T06:30:00Z")
}

func TestRecurringDSTRepeatFirstPassOnly(t *testing.T) {
	dstDue(t, "America/New_York", "30 1 * * *", "2026-11-01T04:00:00Z", "2026-11-01T07:00:00Z", "2026-11-01T05:30:00Z")
	dstDue(t, "America/New_York", "*/20 1 * * *", "2026-11-01T04:00:00Z", "2026-11-01T07:00:00Z", "2026-11-01T05:00:00Z", "2026-11-01T05:20:00Z", "2026-11-01T05:40:00Z")
	for _, cursor := range []string{"2026-11-01T05:40:00Z", "2026-11-01T06:00:00Z", "2026-11-01T06:39:59.999999999Z"} {
		next, err := recurringSchedule(Trigger{Kind: "cron", Cron: "*/20 1 * * *", Timezone: "America/New_York"})
		require.NoError(t, err)
		require.Equal(t, at(t, "2026-11-02T06:00:00Z"), next(at(t, cursor)))
	}
}

func TestRecurringDSTHalfHourZone(t *testing.T) {
	dstDue(t, "Australia/Lord_Howe", "15 2 * * *", "2026-10-03T00:00:00Z", "2026-10-04T00:00:00Z", "2026-10-03T15:30:00Z")
	dstDue(t, "Australia/Lord_Howe", "45 1 * * *", "2026-04-04T12:00:00Z", "2026-04-04T18:00:00Z", "2026-04-04T14:45:00Z")
	dstDue(t, "Australia/Lord_Howe", "0 12 * * *", "2026-04-04T00:00:00Z", "2026-04-06T02:00:00Z", "2026-04-04T01:00:00Z", "2026-04-05T01:30:00Z", "2026-04-06T01:30:00Z")
}

func TestRecurringDSTMidnightSkipped(t *testing.T) {
	dstDue(t, "America/Santiago", "0 0 * * *", "2026-09-06T00:00:00Z", "2026-09-06T08:00:00Z", "2026-09-06T04:00:00Z")
	dstDue(t, "America/Santiago", "0 23 * * *", "2026-04-05T00:00:00Z", "2026-04-05T05:00:00Z", "2026-04-05T02:00:00Z")
	dstDue(t, "America/Havana", "0 0 * * *", "2026-03-08T00:00:00Z", "2026-03-08T08:00:00Z", "2026-03-08T05:00:00Z")
}

func TestRecurringDSTCatchupModes(t *testing.T) {
	for _, mode := range []string{"latest", "skip"} {
		t.Run(mode, func(t *testing.T) {
			j := dstJob(t, "America/New_York", "30 2 * * *")
			j.Catchup, j.GraceSeconds = mode, 60
			a, now := at(t, "2026-03-07T08:00:00Z"), at(t, "2026-03-08T07:00:30Z")
			b, err := PlanRecurring(j, definitionUID, RecurringProgress{Version: 1, Anchor: a, Through: a}, now)
			require.NoError(t, err)
			require.Equal(t, []time.Time{at(t, "2026-03-08T07:00:00Z")}, b.Due)
			replay, err := PlanRecurring(j, definitionUID, b.Progress, now)
			require.NoError(t, err)
			require.Empty(t, replay.Due)
		})
	}
	t.Run("saved 0.19.0 scan", func(t *testing.T) {
		j := dstJob(t, "America/New_York", "*/20 1 * * *")
		j.Catchup = "latest"
		a := at(t, "2025-12-04T06:00:00Z")
		until := at(t, "2026-11-01T06:45:00Z")
		p := RecurringProgress{Version: 1, Anchor: a, Through: a, Scan: &RecurringScan{DefinitionEventUID: definitionUID, Cursor: at(t, "2026-11-01T06:20:00Z"), Until: until}}
		// A later clock must not expand the frozen horizon while restarting.
		for range 5 {
			b, err := PlanRecurring(j, definitionUID, p, until.Add(24*time.Hour))
			require.NoError(t, err)
			require.NotContains(t, b.Due, at(t, "2026-11-01T06:20:00Z"))
			if !b.More {
				require.Equal(t, []time.Time{at(t, "2026-11-01T05:40:00Z")}, b.Due)
				require.Equal(t, until, b.Progress.Through)
				return
			}
			require.Empty(t, b.Due)
			require.Equal(t, until, b.Progress.Scan.Until)
			p = b.Progress
		}
		t.Fatal("scan failed to converge")
	})
}

func TestRecurringDSTProgressFrom0190(t *testing.T) {
	a := at(t, "2026-03-07T08:00:00Z")
	for _, tc := range []struct {
		name, through, mode string
		want                []time.Time
	}{
		{"past gap", "2026-03-08T07:00:10Z", "all", []time.Time{at(t, "2026-03-09T06:30:00Z")}},
		{"empty old sweep", "2026-03-07T08:00:00Z", "all", []time.Time{at(t, "2026-03-08T07:00:00Z"), at(t, "2026-03-09T06:30:00Z")}},
		{"expired gap", "2026-03-07T08:00:00Z", "skip", []time.Time{at(t, "2026-03-09T06:30:00Z")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := dstJob(t, "America/New_York", "30 2 * * *")
			j.Catchup, j.GraceSeconds = tc.mode, 3600
			b, err := PlanRecurring(j, definitionUID, RecurringProgress{Version: 1, Anchor: a, Through: at(t, tc.through)}, at(t, "2026-03-09T07:00:00Z"))
			require.NoError(t, err)
			require.Equal(t, tc.want, b.Due)
		})
	}
}

func TestRecurringDSTWallClockUnchanged(t *testing.T) {
	for _, expr := range []string{"0 * * * *", "0 0-23 * * *", "@hourly"} {
		dstDue(t, "America/New_York", expr, "2026-11-01T04:59:00Z", "2026-11-01T06:00:00Z", "2026-11-01T05:00:00Z", "2026-11-01T06:00:00Z")
		dstDue(t, "America/New_York", expr, "2026-03-08T05:59:00Z", "2026-03-08T07:00:00Z", "2026-03-08T06:00:00Z", "2026-03-08T07:00:00Z")
	}
}

var dstZones = []string{"America/New_York", "Europe/London", "Australia/Sydney", "Australia/Lord_Howe", "America/Santiago", "America/Havana", "Antarctica/Troll"}

func TestRecurringDSTOnePerLocalDay(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		zone := rapid.SampledFrom(dstZones).Draw(t, "zone")
		hour, minute := rapid.IntRange(0, 23).Draw(t, "hour"), rapid.IntRange(0, 59).Draw(t, "minute")
		loc, err := time.LoadLocation(zone)
		require.NoError(t, err)
		next, err := recurringSchedule(Trigger{Kind: "cron", Cron: fmt.Sprintf("%d %d * * *", minute, hour), Timezone: zone})
		require.NoError(t, err)
		// Enumerate naive dates independently; even a skipped midnight belongs to its date.
		date, end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
		cursor := time.Date(2026, 1, 1, 0, 0, 0, 0, loc).Add(-time.Nanosecond)
		for date.Before(end) {
			due := next(cursor)
			require.True(t, due.After(cursor), "cursor %s, due %s", cursor, due)
			require.Equal(t, date.Format(time.DateOnly), due.In(loc).Format(time.DateOnly))
			cursor, date = due, date.AddDate(0, 0, 1)
		}
		require.Equal(t, "2028-01-01", next(cursor).In(loc).Format(time.DateOnly))
	})
}

// Independent oracle: scan actual minutes and match exported parser bitfields.
// Remember first-pass wall times; at a forward jump scan the missing wall minutes.
func dstMinuteOracle(spec *robcron.SpecSchedule, start, end time.Time) []time.Time {
	matches := func(w time.Time) bool {
		dom, dow := spec.Dom&(1<<uint(w.Day())) != 0, spec.Dow&(1<<uint(w.Weekday())) != 0
		day := dom || dow
		if spec.Dom&(1<<63) != 0 || spec.Dow&(1<<63) != 0 {
			day = dom && dow
		}
		return spec.Minute&(1<<uint(w.Minute())) != 0 && spec.Hour&(1<<uint(w.Hour())) != 0 && spec.Month&(1<<uint(w.Month())) != 0 && day
	}
	fixed := spec.Hour&((1<<24)-1) != (1<<24)-1
	seen := map[[3]int]bool{}
	var due []time.Time
	scan := start.Add(-24 * time.Hour).Truncate(time.Minute)
	_, previous := scan.Add(-time.Minute).In(spec.Location).Zone()
	for ; !scan.After(end); scan = scan.Add(time.Minute) {
		wall := scan.In(spec.Location)
		_, offset := wall.Zone()
		hit := false
		if fixed && offset > previous {
			for w := scan.Add(time.Duration(previous) * time.Second); w.Before(scan.Add(time.Duration(offset) * time.Second)); w = w.Add(time.Minute) {
				if matches(w) {
					hit = true
				}
			}
		}
		if matches(wall) {
			key := [3]int{wall.Year(), wall.YearDay(), wall.Hour()*60 + wall.Minute()}
			if !fixed || !seen[key] {
				hit = true
			}
			if fixed {
				seen[key] = true
			}
		}
		if hit && scan.After(start) {
			due = append(due, scan.UTC())
		}
		previous = offset
	}
	return due
}

func TestRecurringDSTMatchesMinuteScan(t *testing.T) {
	for _, tc := range []struct{ zone, spring, fall string }{
		{"America/New_York", "2026-03-07", "2026-10-31"},
		{"Europe/London", "2026-03-28", "2026-10-24"},
		{"Australia/Sydney", "2026-10-03", "2026-04-04"},
		{"Australia/Lord_Howe", "2026-10-03", "2026-04-04"},
		{"America/Santiago", "2026-09-05", "2026-04-04"},
		{"America/Havana", "2026-03-07", "2026-10-31"},
		{"Antarctica/Troll", "2026-03-28", "2026-10-24"},
		{"Asia/Tokyo", "2026-03-07", "2026-10-31"},
	} {
		for _, day := range []string{tc.spring, tc.fall} {
			for _, expr := range []string{"0 0 * * *", "15 2 * * *", "*/20 1 * * *", "0 12 * * *", "15 * * * *", "30 2 9 * 0"} {
				t.Run(tc.zone+day+expr, func(t *testing.T) {
					start := at(t, day+"T00:00:00Z")
					end := start.Add(48 * time.Hour)
					parsed, err := ParseCron("CRON_TZ=" + tc.zone + " " + expr)
					require.NoError(t, err)
					want := dstMinuteOracle(parsed.(*robcron.SpecSchedule), start, end)
					next, err := recurringSchedule(Trigger{Kind: "cron", Cron: expr, Timezone: tc.zone})
					require.NoError(t, err)
					var got []time.Time
					for cursor, i := start, 0; i < 200; i++ {
						d := next(cursor)
						require.True(t, d.After(cursor))
						if d.After(end) {
							break
						}
						got, cursor = append(got, d), d
					}
					require.Equal(t, want, got)
				})
			}
		}
	}
}

// A timer makes a non-advancing synthetic ZoneBounds regression fail promptly.
func TestRecurringDSTTerminates(t *testing.T) {
	for _, tc := range []struct{ zone, start, want string }{
		{"America/New_York", "2040-12-31T00:00:00Z", "2040-12-31T14:00:00Z"},
		{"America/New_York", "2040-12-31T12:00:00Z", "2040-12-31T14:00:00Z"},
		{"America/New_York", "2044-12-31T00:00:00Z", "2044-12-31T14:00:00Z"},
		{"America/New_York", "2044-12-31T12:00:00Z", "2044-12-31T14:00:00Z"},
		{"Asia/Tokyo", "9990-06-01T00:00:00Z", "9990-06-02T00:00:00Z"},
		{"America/New_York", "9990-12-31T00:00:00Z", "9990-12-31T14:00:00Z"},
	} {
		t.Run(tc.start+tc.zone, func(t *testing.T) {
			next, err := recurringSchedule(Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: tc.zone})
			require.NoError(t, err)
			cursor := at(t, tc.start)
			done := make(chan time.Time, 1)
			go func() { done <- next(cursor) }()
			select {
			case got := <-done:
				require.Equal(t, at(t, tc.want), got)
			case <-time.After(2 * time.Second):
				t.Fatal("next did not terminate")
			}
		})
	}
}

// Shrunk daily-property counterexample: robfig loses the transition day
// even at midnight, outside Lord Howe's repeated interval.
func TestRecurringDSTLordHoweMidnight(t *testing.T) {
	dstDue(t, "Australia/Lord_Howe", "0 0 * * *", "2026-04-04T00:00:00Z", "2026-04-05T16:00:00Z", "2026-04-04T13:00:00Z", "2026-04-05T13:30:00Z")
}

func TestRecurringDSTLeapDaySearchBound(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, zone := range []string{"America/New_York", "Australia/Lord_Howe", "Antarctica/Troll", "America/Santiago", "Europe/London"} {
			parsed, err := ParseCron("CRON_TZ=" + zone + " 0 9 29 2 *")
			if !assert.NoError(t, err) {
				return
			}
			spec := parsed.(*robcron.SpecSchedule)
			naive := *spec
			naive.Location = time.UTC
			for year := 2026; year <= 2399; year++ {
				cursor := time.Date(year, 3, 1, 0, 0, 0, 0, time.UTC)
				due, count := dstOccurrence(&naive, true, spec.Location, cursor)
				if !assert.LessOrEqual(t, count, 32, "%s %d", zone, year) {
					return
				}
				// Century years such as 2100 are not leap years: the next
				// February 29 can legitimately fall beyond the five-year horizon.
				leap := year + 1
				for leap%4 != 0 || (leap%100 == 0 && leap%400 != 0) {
					leap++
				}
				if leap > year+5 {
					if !assert.True(t, due.IsZero(), "%s %d", zone, year) {
						return
					}
					continue
				}
				if !assert.False(t, due.IsZero(), "%s %d", zone, year) {
					return
				}
				wall := due.In(spec.Location)
				if !assert.Equal(t, time.February, wall.Month()) || !assert.Equal(t, 29, wall.Day()) || !assert.Equal(t, 9, wall.Hour()) {
					return
				}
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("leap-day enumeration did not terminate")
	}
}

func TestRecurringDSTDescriptorsAndLocations(t *testing.T) {
	for _, expr := range []string{"@daily", "@weekly"} {
		dstDue(t, "America/Havana", expr, "2026-03-08T00:00:00Z", "2026-03-08T08:00:00Z", "2026-03-08T05:00:00Z")
	}
	for _, zone := range []string{"UTC", "Etc/GMT+5"} {
		next, err := recurringSchedule(Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: zone})
		require.NoError(t, err)
		start := at(t, "2026-03-08T00:00:00Z")
		want := "2026-03-08T09:00:00Z"
		if zone == "Etc/GMT+5" {
			want = "2026-03-08T14:00:00Z"
		}
		require.Equal(t, at(t, want), next(start))
	}
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	next, err := recurringSchedule(Trigger{Kind: "cron", Cron: "30 1 * * *", Timezone: "Local"})
	require.NoError(t, err)
	cursor := at(t, "2026-11-01T05:40:00Z").In(loc)
	require.Equal(t, at(t, "2026-11-02T06:30:00Z"), next(cursor))
}
