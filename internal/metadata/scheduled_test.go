package metadata

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScheduledOnDueDateUsesIssueTimezone(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)

	due, err := ScheduledOnDue(`{"scheduled_on":"2026-09-01","timezone":"America/Los_Angeles"}`, now, "")
	require.NoError(t, err)
	assert.False(t, due, "the scheduled local day has not started west of UTC")

	due, err = ScheduledOnDue(`{"scheduled_on":"2026-09-01","timezone":"Asia/Tokyo"}`, now, "")
	require.NoError(t, err)
	assert.True(t, due, "the scheduled local day has started east of UTC")
}

func TestScheduledOnDueDateUsesDaemonTimezoneThenUTC(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)
	metadata := `{"scheduled_on":"2026-09-01"}`

	due, err := ScheduledOnDue(metadata, now, "America/Los_Angeles")
	require.NoError(t, err)
	assert.False(t, due)

	due, err = ScheduledOnDue(metadata, now, "")
	require.NoError(t, err)
	assert.True(t, due)
}

func TestScheduledOnDueLocalTimeUsesTimezone(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)

	due, err := ScheduledOnDue(`{"scheduled_on":"2026-09-01T09:00","timezone":"Asia/Tokyo"}`, now, "")
	require.NoError(t, err)
	assert.True(t, due)

	due, err = ScheduledOnDue(
		`{"scheduled_on":"2026-09-01T09:00","timezone":"America/Los_Angeles"}`,
		now,
		"",
	)
	require.NoError(t, err)
	assert.False(t, due)
}

func TestScheduledOnDueLocalTimeUsesFirstInstantAfterDSTGap(t *testing.T) {
	metadata := `{"scheduled_on":"2026-03-08T02:30","timezone":"America/New_York"}`

	due, err := ScheduledOnDue(metadata, time.Date(2026, 3, 8, 6, 59, 59, 0, time.UTC), "")
	require.NoError(t, err)
	assert.False(t, due)

	// 02:30 does not exist on this day. The gate opens at 03:00 EDT, the first
	// valid instant after the gap.
	due, err = ScheduledOnDue(metadata, time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC), "")
	require.NoError(t, err)
	assert.True(t, due)
}

func TestScheduledOnDueLocalTimeStaysDueAcrossDSTOverlap(t *testing.T) {
	metadata := `{"scheduled_on":"2026-11-01T01:30","timezone":"America/New_York"}`

	tests := []struct {
		name string
		now  time.Time
		due  bool
	}{
		{name: "before first occurrence", now: time.Date(2026, 11, 1, 5, 29, 59, 0, time.UTC), due: false},
		{name: "at first occurrence", now: time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), due: true},
		{name: "after clock repeats", now: time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC), due: true},
		{name: "at second occurrence", now: time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), due: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			due, err := ScheduledOnDue(metadata, tt.now, "")
			require.NoError(t, err)
			assert.Equal(t, tt.due, due)
		})
	}
}

func TestScheduledOnDueLocalTimeUsesFirstInstantAfterSkippedDay(t *testing.T) {
	metadata := `{"scheduled_on":"2011-12-30T12:00","timezone":"Pacific/Apia"}`

	due, err := ScheduledOnDue(metadata, time.Date(2011, 12, 30, 9, 59, 59, 0, time.UTC), "")
	require.NoError(t, err)
	assert.False(t, due)

	due, err = ScheduledOnDue(metadata, time.Date(2011, 12, 30, 10, 0, 0, 0, time.UTC), "")
	require.NoError(t, err)
	assert.True(t, due)
}

func TestScheduledOnDueUTCInstant(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 30, 0, 500_000_000, time.UTC)

	tests := []struct {
		name      string
		value     string
		timezone  string
		wantReady bool
	}{
		{name: "same instant", value: "2026-09-01T00:30:00.500Z", wantReady: true},
		{name: "future instant", value: "2026-09-01T00:30:01Z", wantReady: false},
		{name: "timezone ignored for instant", value: "2026-09-01T00:30:00Z", timezone: "Pacific/Apia", wantReady: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]string{"scheduled_on": tt.value, "timezone": tt.timezone})
			require.NoError(t, err)
			due, err := ScheduledOnDue(string(raw), now, "America/Los_Angeles")
			require.NoError(t, err)
			assert.Equal(t, tt.wantReady, due)
		})
	}
}

func TestScheduledOnDueRejectsNumericOffset(t *testing.T) {
	_, err := ScheduledOnDue(
		`{"scheduled_on":"2026-09-01T14:30:00+14:00"}`,
		time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC),
		"UTC",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UTC with Z")
}

func TestScheduledOnDueRejectsInvalidExplicitTimezone(t *testing.T) {
	_, err := ScheduledOnDue(
		`{"scheduled_on":"2026-09-01","timezone":"Not/AZone"}`,
		time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		"UTC",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Not/AZone")
}

func TestScheduledOnDueWithoutScheduleIsDue(t *testing.T) {
	due, err := ScheduledOnDue(`{"timezone":"America/Los_Angeles"}`, time.Now(), "")
	require.NoError(t, err)
	assert.True(t, due)
}

func TestScheduledOnCalendarDateUsesDisplayTimezoneForTimedValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "UTC instant moves to previous browser day",
			raw:  `{"scheduled_on":"2026-09-01T00:30:00Z"}`,
			want: "2026-08-31",
		},
		{
			name: "local time resolves before browser projection",
			raw:  `{"scheduled_on":"2026-09-01T09:00","timezone":"Asia/Tokyo"}`,
			want: "2026-08-31",
		},
		{
			name: "date keeps its civil day",
			raw:  `{"scheduled_on":"2026-09-01","timezone":"Asia/Tokyo"}`,
			want: "2026-09-01",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, present, err := ScheduledOnCalendarDate(tt.raw, "America/Los_Angeles", "UTC")
			require.NoError(t, err)
			assert.True(t, present)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestScheduledOnCalendarDateWithoutSchedule(t *testing.T) {
	date, present, err := ScheduledOnCalendarDate(`{"deadline_on":"2026-09-01"}`, "UTC", "")
	require.NoError(t, err)
	assert.False(t, present)
	assert.Empty(t, date)
}
