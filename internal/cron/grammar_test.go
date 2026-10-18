package cron

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These cases pin ParseCron's integration with the cron dialect documented in
// docs/reference/cron.md#cron-expression-grammar. Changing an example's behavior
// means updating the page too: steps select field values, not elapsed intervals.
func TestParseCronStepsRestartEachPeriod(t *testing.T) {
	for _, tt := range []struct {
		name  string
		expr  string
		start string
		want  []string
	}{
		{
			name: "minute step restarts at the hour", expr: "*/7 * * * *",
			start: "2026-01-01T00:50:00Z",
			want:  []string{"2026-01-01T00:56:00Z", "2026-01-01T01:00:00Z"},
		},
		{
			name: "hour step restarts at midnight", expr: "0 */5 * * *",
			start: "2026-01-01T19:00:00Z",
			want:  []string{"2026-01-01T20:00:00Z", "2026-01-02T00:00:00Z"},
		},
		{
			name: "day step restarts at the month", expr: "0 0 */2 * *",
			start: "2026-01-30T00:00:00Z",
			want:  []string{"2026-01-31T00:00:00Z", "2026-02-01T00:00:00Z"},
		},
		{
			name: "weekday step restarts on Sunday", expr: "0 0 * * */2",
			start: "2026-01-02T00:00:00Z",
			want:  []string{"2026-01-03T00:00:00Z", "2026-01-04T00:00:00Z"},
		},
		{
			name: "month step restarts at the year", expr: "0 0 1 */5 *",
			start: "2026-01-01T00:00:00Z",
			want:  []string{"2026-06-01T00:00:00Z", "2026-11-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		},
		{
			name: "single value step extends to the field end", expr: "5/15 * * * *",
			start: "2026-01-01T00:00:00Z",
			want:  []string{"2026-01-01T00:05:00Z", "2026-01-01T00:20:00Z", "2026-01-01T00:35:00Z", "2026-01-01T00:50:00Z"},
		},
		{
			name: "weekly descriptor selects Sunday", expr: "@weekly",
			start: "2026-01-01T00:00:00Z",
			want:  []string{"2026-01-04T00:00:00Z"},
		},
		{
			name: "named weekday range supports steps", expr: "0 0 * * mon-fri/2",
			start: "2026-01-04T00:00:00Z",
			want:  []string{"2026-01-05T00:00:00Z", "2026-01-07T00:00:00Z", "2026-01-09T00:00:00Z"},
		},
		{
			name: "bounded range restarts at its first value", expr: "10-40/7 * * * *",
			start: "2026-01-01T00:31:00Z",
			want:  []string{"2026-01-01T00:38:00Z", "2026-01-01T01:10:00Z"},
		},
		{
			name: "dividing a bounded range leaves a gap", expr: "0-29/15 * * * *",
			start: "2025-12-31T23:59:00Z",
			want:  []string{"2026-01-01T00:00:00Z", "2026-01-01T00:15:00Z", "2026-01-01T01:00:00Z"},
		},
		{
			name: "oversized step selects the first value", expr: "*/60 * * * *",
			start: "2025-12-31T23:59:00Z",
			want:  []string{"2026-01-01T00:00:00Z", "2026-01-01T01:00:00Z"},
		},
		{
			name: "day step skips months without its value", expr: "0 0 31/31 * *",
			start: "2026-01-01T00:00:00Z",
			want:  []string{"2026-01-31T00:00:00Z", "2026-03-31T00:00:00Z"},
		},
		{
			name: "question mark is a shared parser wildcard", expr: "0 9 ? * *",
			start: "2026-01-01T00:00:00Z",
			want:  []string{"2026-01-01T09:00:00Z", "2026-01-02T09:00:00Z"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			schedule, err := ParseCron(tt.expr)
			require.NoError(t, err)
			current, err := time.Parse(time.RFC3339, tt.start)
			require.NoError(t, err)
			for _, want := range tt.want {
				current = schedule.Next(current)
				require.Equal(t, want, current.UTC().Format(time.RFC3339), tt.expr)
			}
		})
	}

	t.Run("descriptors are lowercase", func(t *testing.T) {
		_, err := ParseCron("@DAILY")
		require.Error(t, err)
	})
}
