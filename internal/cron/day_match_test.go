package cron

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func octoberDayMatches(t testing.TB, j JobDefinition) []time.Time {
	t.Helper()
	anchor := at(t, "2026-09-30T00:00:00Z")
	batch, err := PlanRecurring(j, definitionUID, RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}, at(t, "2026-11-01T00:00:00Z"))
	require.NoError(t, err)
	require.False(t, batch.More)
	return batch.Due
}

func octoberEitherDay(t testing.TB) []time.Time {
	t.Helper()
	var want []time.Time
	for _, day := range []int{1, 2, 3, 4, 5, 6, 7, 13, 20, 27} {
		want = append(want, time.Date(2026, 10, day, 9, 0, 0, 0, time.UTC))
	}
	return want
}

// Tightening writes must not stop a definition stored by an earlier release.
func TestPlanRecurringKeepsStoredBothDayFieldsAsAny(t *testing.T) {
	j := nativeJob(t)
	j.Trigger = Trigger{Kind: "cron", Cron: "0 9 1-7 * 2", Timezone: "UTC"}
	require.Equal(t, octoberEitherDay(t), octoberDayMatches(t, j))
}

func TestAdapterJobKeepsBothDayFields(t *testing.T) {
	j, err := ParseJob([]byte(executeJSON))
	require.NoError(t, err)
	j.Trigger = Trigger{Kind: "cron", Cron: "0 9 1-7 * 2", Timezone: "UTC"}
	raw, err := json.Marshal(j)
	require.NoError(t, err)
	_, err = ParseJob(raw)
	require.NoError(t, err)
}

func nativeDayMatchJSON(expression, zone, member string) []byte {
	trigger := fmt.Sprintf(`{"kind":"cron","cron":%q,"timezone":%q%s}`, expression, zone, member)
	return []byte(strings.Replace(recurringJSON, `{"kind":"interval","interval_seconds":7200}`, trigger, 1))
}

func TestNativeCronBothDayFieldsRequireDayMatch(t *testing.T) {
	for _, tc := range []struct {
		expression string
		reject     bool
	}{
		{"0 9 1-7 * 2", true}, {"0 9 1-7 * TUE", true},
		{"0 9 */2 * 1", true}, {"0 9 1 * 0-6", true}, {"0 9 1-31 * 2", true},
		{"0 9 1-7 * *", false}, {"0 9 1-7 * ?", false}, {"0 9 * * 2", false},
		{"0 9 ? * 2", false}, {"0 9 */1 * 2", false}, {"0 9 *,5 * 2", false},
		{"@weekly", false}, {"@monthly", false},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			_, err := ParseJob(nativeDayMatchJSON(tc.expression, "UTC", ""))
			if tc.reject {
				require.ErrorIs(t, err, ErrInvalid)
				require.ErrorContains(t, err, "trigger.day_match")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestNativeCronDayMatchAllIsFirstWeekday(t *testing.T) {
	j, err := ParseJob(nativeDayMatchJSON("0 9 1-7 * 2", "UTC", `,"day_match":"all"`))
	require.NoError(t, err)
	anchor := at(t, "2026-09-30T00:00:00Z")
	batch, err := PlanRecurring(j, definitionUID, RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}, at(t, "2026-12-31T00:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, []time.Time{at(t, "2026-10-06T09:00:00Z"), at(t, "2026-11-03T09:00:00Z"), at(t, "2026-12-01T09:00:00Z")}, batch.Due)
}

func TestDayMatchRejections(t *testing.T) {
	for _, tc := range []struct{ name, expression, zone, member, message string }{
		{"one unrestricted", "0 9 * * 2", "UTC", "all", "applies only when"},
		{"bad value", "0 9 1-7 * 2", "UTC", "both", `must be "any" or "all"`},
		{"leap Monday", "0 9 29 2 1", "UTC", "all", "deliverable occurrence every year"},
		{"January Sunday", "0 9 1 1 0", "UTC", "all", "deliverable occurrence every year"},
		{"impossible AND", "0 9 30 2 1", "UTC", "all", "deliverable occurrence every year"},
		{"spring gap", "30 2 8-14 3 0", "America/New_York", "all", "deliverable occurrence every year"},
		{"Friday thirteenth", "0 9 13 * 5", "UTC", "all", ""},
		{"first Sunday", "30 2 1-7 * 0", "America/New_York", "all", ""},
		{"zone directive", "CRON_TZ=America/New_York 0 9 1-7 * 2", "America/New_York", "all", ""},
		{"January midnight", "0 0 1 1 0-6", "Pacific/Auckland", "all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseJob(nativeDayMatchJSON(tc.expression, tc.zone, fmt.Sprintf(`,"day_match":%q`, tc.member)))
			if tc.message == "" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrInvalid)
				require.ErrorContains(t, err, tc.message)
			}
		})
	}
	t.Run("interval", func(t *testing.T) {
		raw := strings.Replace(recurringJSON, `"interval_seconds":7200`, `"interval_seconds":7200,"day_match":"all"`, 1)
		_, err := ParseJob([]byte(raw))
		require.ErrorContains(t, err, "trigger carries fields for another kind")
	})
	t.Run("adapter", func(t *testing.T) {
		raw := strings.Replace(string(nativeDayMatchJSON("0 9 1-7 * 2", "UTC", `,"day_match":"all"`)), `"action":{"kind":"notify","recipient":"current-owner-or-author"}`, `"action":{"kind":"execute","prompt":"Review"}`, 1)
		_, err := ParseJob([]byte(raw))
		require.ErrorContains(t, err, "only supported for native recurring issue jobs")
	})
}

// Built-in fuzzing varies the weekday and mode; civil dates independently
// specify the contract, catching a lost AND bit or an accidental OR rewrite.
func FuzzNativeCronDayMatch(f *testing.F) {
	for weekday := range 7 {
		f.Add(byte(weekday), true)
		f.Add(byte(weekday), false)
	}
	f.Fuzz(func(t *testing.T, weekday byte, all bool) {
		weekday %= 7
		mode := "any"
		if all {
			mode = "all"
		}
		j, err := ParseJob(nativeDayMatchJSON(fmt.Sprintf("0 9 1-7 * %d", weekday), "UTC", fmt.Sprintf(`,"day_match":%q`, mode)))
		require.NoError(t, err)
		var want []time.Time
		// The anchor is September 30 at midnight, so Wednesday's OR schedule
		// also includes that morning before the October occurrences.
		for day := 0; day <= 31; day++ {
			date := time.Date(2026, 10, day, 9, 0, 0, 0, time.UTC)
			if all && date.Day() <= 7 && date.Weekday() == time.Weekday(weekday) || !all && (date.Day() <= 7 || date.Weekday() == time.Weekday(weekday)) {
				want = append(want, date)
			}
		}
		require.Equal(t, want, octoberDayMatches(t, j))
	})
}
