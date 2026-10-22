package cron

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func cronOccurrences(t *testing.T, expression string, start, end time.Time) []time.Time {
	t.Helper()
	schedule, err := ParseCron(expression)
	require.NoError(t, err)
	var due []time.Time
	for cursor := start; ; {
		next := schedule.Next(cursor)
		if next.IsZero() || next.After(end) {
			return due
		}
		due = append(due, next.UTC())
		cursor = next
	}
}

func TestParseCronAcceptsSevenAsSunday(t *testing.T) {
	// 2026-10-16 is a Friday and 2026-10-18 a Sunday.
	start, end := at(t, "2026-10-12T00:00:00Z"), at(t, "2026-10-19T00:00:00Z")
	for _, tc := range []struct {
		expression string
		want       []string
	}{
		{"0 9 * * 7", []string{"2026-10-18T09:00:00Z"}},
		{"0 9 * * 5-7", []string{"2026-10-16T09:00:00Z", "2026-10-17T09:00:00Z", "2026-10-18T09:00:00Z"}},
		{"0 9 * * 5-7/2", []string{"2026-10-16T09:00:00Z", "2026-10-18T09:00:00Z"}},
		{"CRON_TZ=UTC 0 9 * * 7", []string{"2026-10-18T09:00:00Z"}},
		{"CRON_TZ=UTC 0 9 * * 5-7", []string{"2026-10-16T09:00:00Z", "2026-10-17T09:00:00Z", "2026-10-18T09:00:00Z"}},
		{"CRON_TZ=UTC 0 9 * * 5-7/2", []string{"2026-10-16T09:00:00Z", "2026-10-18T09:00:00Z"}},
		{"CRON_TZ=America/New_York 0 9 * * 7", []string{"2026-10-18T13:00:00Z"}},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			var want []time.Time
			for _, s := range tc.want {
				want = append(want, at(t, s))
			}
			require.Equal(t, want, cronOccurrences(t, tc.expression, start, end))
		})
	}
}

func TestNativeRecurringSundaySeven(t *testing.T) {
	job := nativeJob(t)
	job.Trigger = Trigger{Kind: "cron", Cron: "0 9 * * 7", Timezone: "America/New_York"}
	require.NoError(t, job.Validate())
	anchor := at(t, "2026-10-17T00:00:00Z")
	batch, err := PlanRecurring(job, definitionUID, RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}, at(t, "2026-10-19T00:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, []time.Time{at(t, "2026-10-18T13:00:00Z")}, batch.Due)
}

func TestCronJobKeepsSevenAsWritten(t *testing.T) {
	for _, expression := range []string{"0 9 * * 5-7", "CRON_TZ=UTC 0 9 * * 5-7/2"} {
		t.Run(expression, func(t *testing.T) {
			document := strings.Replace(executeJSON, `"kind":"manual"`, `"kind":"cron","cron":"`+expression+`"`, 1)
			job, err := ParseJob([]byte(document))
			require.NoError(t, err)
			raw, err := json.Marshal(job)
			require.NoError(t, err)
			require.Contains(t, string(raw), `"cron":"`+expression+`"`)
		})
	}
}

// Stored jobs restricting both day fields keep crontab's either-day meaning.
func TestParseCronMatchesDayOfMonthOrDayOfWeek(t *testing.T) {
	// 2026-10-15 is a Thursday; 2026-10-12 and 2026-10-19 are Mondays.
	got := cronOccurrences(t, "0 9 15 * 1", at(t, "2026-10-12T00:00:00Z"), at(t, "2026-10-20T00:00:00Z"))
	require.Equal(t, []time.Time{at(t, "2026-10-12T09:00:00Z"), at(t, "2026-10-15T09:00:00Z"), at(t, "2026-10-19T09:00:00Z")}, got)
}
