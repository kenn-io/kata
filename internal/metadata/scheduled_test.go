package metadata

import (
	"encoding/json"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStampScheduleZonesChoices(t *testing.T) {
	t.Setenv("TZ", "Europe/Paris")
	for _, tc := range []struct {
		name, patch, current, fallback, want string
	}{
		{"daemon fallback", `{"scheduled_on":"2026-01-15"}`, `{}`, "Europe/Paris", `{"scheduled_on":"2026-01-15[Europe/Paris]"}`},
		{"UTC fallback", `{"deadline_on":"2026-01-15"}`, `{}`, "", `{"deadline_on":"2026-01-15[UTC]"}`},
		{"saved zone on edit", `{"scheduled_on":"2026-01-15T10:00"}`, `{"scheduled_on":"2026-01-15T09:00[Europe/Paris]","timezone":"Asia/Tokyo"}`, "UTC", `{"scheduled_on":"2026-01-15T10:00[Europe/Paris]"}`},
		{"patched timezone overrides saved schedule zone", `{"scheduled_on":"2026-01-15T10:00","timezone":"Asia/Tokyo"}`, `{"scheduled_on":"2026-01-15T09:00[Europe/Paris]"}`, "UTC", `{"scheduled_on":"2026-01-15T10:00[Asia/Tokyo]","timezone":"Asia/Tokyo"}`},
		{"patched timezone stamps unchanged legacy date", `{"scheduled_on":"2026-01-15","timezone":"Asia/Tokyo"}`, `{"scheduled_on":"2026-01-15","timezone":"Europe/Paris"}`, "UTC", `{"scheduled_on":"2026-01-15[Asia/Tokyo]","timezone":"Asia/Tokyo"}`},
		{"timezone change rebinds existing dates", `{"timezone":"Asia/Tokyo"}`, `{"scheduled_on":"2026-01-15T09:00[Europe/Paris]","deadline_on":"2026-01-16[Europe/Paris]"}`, "UTC", `{"scheduled_on":"2026-01-15T09:00[Asia/Tokyo]","deadline_on":"2026-01-16[Asia/Tokyo]","timezone":"Asia/Tokyo"}`},
		{"timezone clear rebinds existing dates", `{"timezone":null}`, `{"scheduled_on":"2026-01-15T09:00[Europe/Paris]","deadline_on":"2026-01-16[Europe/Paris]"}`, "", `{"scheduled_on":"2026-01-15T09:00[UTC]","deadline_on":"2026-01-16[UTC]","timezone":null}`},
		{"timezone clear uses daemon zone", `{"timezone":null}`, `{"deadline_on":"2026-01-16[Europe/Paris]"}`, "Asia/Tokyo", `{"deadline_on":"2026-01-16[Asia/Tokyo]","timezone":null}`},
		{"timezone change leaves instant alone", `{"timezone":"Asia/Tokyo"}`, `{"scheduled_on":"2026-01-15T09:00:00Z"}`, "UTC", `{"timezone":"Asia/Tokyo"}`},
		{"explicit date zone wins during rebinding", `{"timezone":"Asia/Tokyo","scheduled_on":"2026-01-17[America/New_York]"}`, `{"scheduled_on":"2026-01-15[Europe/Paris]","deadline_on":"2026-01-16[Europe/Paris]"}`, "UTC", `{"scheduled_on":"2026-01-17[America/New_York]","deadline_on":"2026-01-16[Asia/Tokyo]","timezone":"Asia/Tokyo"}`},
		{"cleared timezone uses default", `{"scheduled_on":"2026-01-16","timezone":null}`, `{"scheduled_on":"2026-01-15[Europe/Paris]"}`, "UTC", `{"scheduled_on":"2026-01-16[UTC]","timezone":null}`},
		{"cleared timezone uses default when no date zone", `{"scheduled_on":"2026-01-16","timezone":null}`, `{"scheduled_on":"2026-01-15","timezone":"Europe/Paris"}`, "Asia/Tokyo", `{"scheduled_on":"2026-01-16[Asia/Tokyo]","timezone":null}`},
		{"date zone overrides patched timezone", `{"scheduled_on":"2026-01-16[Europe/Paris]","timezone":"Asia/Tokyo"}`, `{}`, "UTC", `{"scheduled_on":"2026-01-16[Europe/Paris]","timezone":"Asia/Tokyo"}`},
		{"sibling uses issue zone", `{"deadline_on":"2026-01-16"}`, `{"scheduled_on":"2026-01-15T09:00[Europe/Paris]","timezone":"Asia/Tokyo"}`, "UTC", `{"deadline_on":"2026-01-16[Asia/Tokyo]"}`},
		{"local zone in patch", `{"scheduled_on":"2026-01-15","timezone":"Local"}`, `{}`, "UTC", `{"scheduled_on":"2026-01-15[Europe/Paris]","timezone":"Local"}`},
		{"local zone in current metadata", `{"scheduled_on":"2026-01-15"}`, `{"timezone":"Local"}`, "UTC", `{"scheduled_on":"2026-01-15[Europe/Paris]"}`},
		{"UTC instant", `{"scheduled_on":"2026-01-15T10:00:00Z"}`, `{}`, "UTC", `{"scheduled_on":"2026-01-15T10:00:00Z"}`},
		{"clear", `{"scheduled_on":null}`, `{}`, "Local", `{"scheduled_on":null}`},
		{"unchanged legacy", `{"scheduled_on":"2026-01-15"}`, `{"scheduled_on":"2026-01-15"}`, "UTC", `{"scheduled_on":"2026-01-15[UTC]"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patch map[string]jsontext.Value
			require.NoError(t, json.Unmarshal([]byte(tc.patch), &patch))
			stamped, err := StampScheduleZones(patch, tc.current, tc.fallback)
			require.NoError(t, err)
			encoded, err := json.Marshal(stamped)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(encoded))
		})
	}
}

func TestScheduleDateCarriesItsTimezone(t *testing.T) {
	raw := `{"scheduled_on":"2026-01-15T09:00[Europe/Paris]","deadline_on":"2026-01-15[Asia/Tokyo]","timezone":"America/New_York"}`
	for _, fallback := range []string{"UTC", "Asia/Tokyo"} {
		_, zone, instant, present, err := ScheduleFieldInstant(raw, "scheduled_on", fallback)
		require.NoError(t, err)
		require.True(t, present)
		assert.Equal(t, "Europe/Paris", zone)
		assert.Equal(t, time.Date(2026, 1, 15, 8, 0, 0, 0, time.UTC), instant)
	}

	_, zone, instant, present, err := ScheduleFieldInstant(raw, "deadline_on", "UTC")
	require.NoError(t, err)
	require.True(t, present)
	assert.Equal(t, "Asia/Tokyo", zone)
	assert.Equal(t, time.Date(2026, 1, 14, 15, 0, 0, 0, time.UTC), instant)
}

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

func TestDeadlineOnCalendarDateUsesDisplayTimezoneForTimedValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "UTC instant moves to previous browser day",
			raw:  `{"deadline_on":"2026-09-01T00:30:00Z"}`,
			want: "2026-08-31",
		},
		{
			name: "local time resolves before browser projection",
			raw:  `{"deadline_on":"2026-09-01T09:00","timezone":"Asia/Tokyo"}`,
			want: "2026-08-31",
		},
		{
			name: "date keeps its civil day",
			raw:  `{"deadline_on":"2026-09-01","timezone":"Asia/Tokyo"}`,
			want: "2026-09-01",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, present, err := DeadlineOnCalendarDate(tt.raw, "America/Los_Angeles", "UTC")
			require.NoError(t, err)
			assert.True(t, present)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDeadlineOnCalendarDateWithoutDeadline(t *testing.T) {
	date, present, err := DeadlineOnCalendarDate(`{"scheduled_on":"2026-09-01"}`, "UTC", "")
	require.NoError(t, err)
	assert.False(t, present)
	assert.Empty(t, date)
}

func TestScheduleFieldDueEvaluatesEitherPlanningField(t *testing.T) {
	now := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	raw := `{"scheduled_on":"2026-09-16T09:01","deadline_on":"2026-09-16T09:00"}`

	value, present, due, err := ScheduleFieldDue(raw, "scheduled_on", now, "UTC")
	require.NoError(t, err)
	assert.Equal(t, "2026-09-16T09:01", value)
	assert.True(t, present)
	assert.False(t, due)

	value, present, due, err = ScheduleFieldDue(raw, "deadline_on", now, "UTC")
	require.NoError(t, err)
	assert.Equal(t, "2026-09-16T09:00", value)
	assert.True(t, present)
	assert.True(t, due)

	value, present, due, err = ScheduleFieldDue(`{"timezone":"UTC"}`, "deadline_on", now, "")
	require.NoError(t, err)
	assert.Empty(t, value)
	assert.False(t, present)
	assert.False(t, due)
}

func TestScheduleFieldDueRejectsUnknownField(t *testing.T) {
	_, _, _, err := ScheduleFieldDue(`{}`, "reminder_on", time.Now(), "UTC")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "planning field")
}

func TestCalendarDateReadsTheNamedField(t *testing.T) {
	raw := `{"scheduled_on":"2026-09-01","deadline_on":"2026-09-02"}`
	date, present, err := CalendarDate(raw, "deadline_on", nil, "")
	require.NoError(t, err)
	assert.True(t, present)
	assert.Equal(t, "2026-09-02", date)
}

func TestCalendarDateIgnoresAnUnreadableSiblingField(t *testing.T) {
	raw := `{"scheduled_on":42,"deadline_on":"2026-11-02"}`
	date, present, err := CalendarDate(raw, "deadline_on", nil, "")
	require.NoError(t, err)
	assert.True(t, present)
	assert.Equal(t, "2026-11-02", date)

	_, _, err = CalendarDate(raw, "scheduled_on", nil, "")
	require.Error(t, err)
}

func TestScheduleDueCutoffCoversEveryZone(t *testing.T) {
	t.Run("36-hour boundary", func(t *testing.T) {
		now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
		cutoff := ScheduleDueCutoff(now)
		for _, value := range []string{"2026-09-17", "2026-09-17T00:00", "2026-09-17T00:00:00"} {
			raw, err := json.Marshal(map[string]string{"scheduled_on": value, "timezone": "Etc/GMT+12"})
			require.NoError(t, err)
			_, _, at, present, err := ScheduleFieldInstant(string(raw), "scheduled_on", "UTC")
			require.NoError(t, err)
			require.True(t, present)
			require.Equal(t, now.Add(36*time.Hour), at)
			assert.Greater(t, value, cutoff, "issues dated exactly 36 hours out must be excluded")
		}
	})
	for _, now := range []time.Time{
		time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC),
	} {
		cutoff := ScheduleDueCutoff(now)
		assert.Greater(t, "2099-01-01", cutoff, "distant future dates must be excluded")
		for _, offset := range []time.Duration{-25 * time.Hour, -time.Second, 0, time.Second, 25 * time.Hour} {
			value := now.Add(offset).UTC().Format(time.RFC3339)
			for _, field := range []string{"scheduled_on", "deadline_on"} {
				raw, err := json.Marshal(map[string]string{field: value})
				require.NoError(t, err)
				_, present, due, err := ScheduleFieldDue(string(raw), field, now, "UTC")
				require.NoError(t, err)
				require.True(t, present)
				if due {
					assert.LessOrEqual(t, value, cutoff, "%s at %s", value, now)
				}
			}
		}
		for _, zone := range []string{"UTC", "Pacific/Kiritimati", "Etc/GMT-14", "America/Los_Angeles"} {
			t.Run(zone, func(t *testing.T) {
				location, err := time.LoadLocation(zone)
				require.NoError(t, err)
				values := []string{"2026-03-08T02:30"}
				for _, offset := range []time.Duration{-25 * time.Hour, -time.Second, 0, time.Second, 25 * time.Hour} {
					instant := now.Add(offset).In(location)
					values = append(values,
						instant.Format(time.DateOnly),
						instant.Format("2006-01-02T15:04"),
						instant.Format("2006-01-02T15:04:05"),
					)
				}
				for _, value := range values {
					for _, field := range []string{"scheduled_on", "deadline_on"} {
						raw, err := json.Marshal(map[string]string{field: value, "timezone": zone})
						require.NoError(t, err)
						_, present, due, err := ScheduleFieldDue(string(raw), field, now, "UTC")
						require.NoError(t, err)
						require.True(t, present)
						if due {
							assert.LessOrEqual(t, value, cutoff, "%s in %s at %s", value, zone, now)
						}
					}
				}
			})
		}
	}
}
