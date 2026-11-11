package db

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/metadata"
)

func TestTodayUsesSavedZoneRatherThanViewerDay(t *testing.T) {
	raw := `{"scheduled_on":{"value":"2026-01-01","timezone":"Asia/Tokyo","tzdata_version":"` + metadata.ScheduleTZDataVersion + `"}}`
	query := UISnapshotQuery{View: "today", TimeZone: "America/Los_Angeles", LocalDate: "2025-12-31", CalendarAt: "2026-01-01T01:00:00Z", DefaultTimezone: "UTC"}
	_, _, _, _, included, err := MatchUICalendarView(raw, query, "UTC")
	require.NoError(t, err)
	require.True(t, included)
}

func TestRowPlanningDatesResolveInIssueThenDefaultThenUTC(t *testing.T) {
	tests := []struct {
		name            string
		raw             string
		scheduledZone   string
		deadlineZone    string
		wantScheduledOn string
		wantDeadlineOn  string
	}{
		{
			name:            "date-only values keep their written date in any zone",
			raw:             `{"scheduled_on":"2026-10-29","deadline_on":"2026-11-02","timezone":"Asia/Tokyo"}`,
			scheduledZone:   "America/Los_Angeles",
			deadlineZone:    "America/Los_Angeles",
			wantScheduledOn: "2026-10-29",
			wantDeadlineOn:  "2026-11-02",
		},
		{
			// 02:00 on the 29th in Auckland (UTC+13) is still the 28th in UTC.
			name:            "local date-time resolves in the daemon zone",
			raw:             `{"scheduled_on":"2026-10-29T02:00","deadline_on":"2026-10-29T02:00"}`,
			scheduledZone:   "Pacific/Auckland",
			deadlineZone:    "Pacific/Auckland",
			wantScheduledOn: "2026-10-29",
			wantDeadlineOn:  "2026-10-29",
		},
		{
			name:            "local date-time with no zone anywhere reads as UTC",
			raw:             `{"scheduled_on":"2026-10-29T23:30","deadline_on":"2026-10-29T23:30"}`,
			wantScheduledOn: "2026-10-29",
			wantDeadlineOn:  "2026-10-29",
		},
		{
			name:            "RFC 3339 instant falls on the issue zone date",
			raw:             `{"scheduled_on":"2026-10-29T23:30:00Z","deadline_on":"2026-10-29T23:30:00Z","timezone":"Asia/Tokyo"}`,
			scheduledZone:   "America/Los_Angeles",
			deadlineZone:    "America/Los_Angeles",
			wantScheduledOn: "2026-10-30",
			wantDeadlineOn:  "2026-10-30",
		},
		{
			name:            "RFC 3339 instant falls on the daemon zone date without an issue zone",
			raw:             `{"scheduled_on":"2026-10-29T03:30:00Z","deadline_on":"2026-10-29T03:30:00Z"}`,
			scheduledZone:   "America/Los_Angeles",
			deadlineZone:    "America/Los_Angeles",
			wantScheduledOn: "2026-10-28",
			wantDeadlineOn:  "2026-10-28",
		},
		{
			name:            "RFC 3339 instant falls on the UTC date with no zone anywhere",
			raw:             `{"scheduled_on":"2026-10-29T23:30:00Z","deadline_on":"2026-10-29T23:30:00Z"}`,
			wantScheduledOn: "2026-10-29",
			wantDeadlineOn:  "2026-10-29",
		},
		{
			name:            "scheduled and deadline fall back to their own default zones",
			raw:             `{"scheduled_on":"2026-10-29T23:30:00Z","deadline_on":"2026-10-29T23:30:00Z"}`,
			scheduledZone:   "Asia/Tokyo",
			deadlineZone:    "America/Los_Angeles",
			wantScheduledOn: "2026-10-30",
			wantDeadlineOn:  "2026-10-29",
		},
		{
			// 04:30Z is 00:30 EDT on the 9th the day after the spring-forward
			// change; a fixed standard-time offset would give the 8th.
			name:            "instant just after the spring-forward change uses the daylight offset",
			raw:             `{"scheduled_on":"2026-03-09T04:30:00Z","timezone":"America/New_York"}`,
			wantScheduledOn: "2026-03-09",
		},
		{
			name:            "instant just before the spring-forward change uses the standard offset",
			raw:             `{"scheduled_on":"2026-03-08T04:30:00Z","timezone":"America/New_York"}`,
			wantScheduledOn: "2026-03-07",
		},
		{
			name:           "local date-time inside the skipped hour keeps its civil date",
			raw:            `{"deadline_on":"2026-03-08T02:30","timezone":"America/New_York"}`,
			wantDeadlineOn: "2026-03-08",
		},
		{
			name:           "only the present key is reported",
			raw:            `{"deadline_on":"2026-11-02"}`,
			wantDeadlineOn: "2026-11-02",
		},
		{
			name: "metadata with no planning keys reports neither",
			raw:  `{"timezone":"Asia/Tokyo","topic":"x"}`,
		},
		{
			name: "empty metadata reports neither",
			raw:  ``,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheduledOn, deadlineOn, err := RowPlanningDates(tt.raw, tt.scheduledZone, tt.deadlineZone)
			require.NoError(t, err)
			assert.Equal(t, tt.wantScheduledOn, scheduledOn)
			assert.Equal(t, tt.wantDeadlineOn, deadlineOn)
		})
	}
}

func TestRowPlanningDatesOmitsOnlyTheUnreadableField(t *testing.T) {
	scheduledOn, deadlineOn, err := RowPlanningDates(`{"scheduled_on":"next tuesday","deadline_on":"2026-11-02"}`, "", "")
	require.NoError(t, err)
	assert.Empty(t, scheduledOn)
	assert.Equal(t, "2026-11-02", deadlineOn)

	scheduledOn, deadlineOn, err = RowPlanningDates(`{"scheduled_on":"2026-10-29","timezone":"Mars/Base","deadline_on":"2026-10-29T23:30:00Z"}`, "", "")
	require.NoError(t, err)
	assert.Equal(t, "2026-10-29", scheduledOn)
	assert.Empty(t, deadlineOn, "an unknown issue zone cannot place an instant")
}

func TestRowPlanningDatesKeepsAValidDateBesideANonStringOne(t *testing.T) {
	scheduledOn, deadlineOn, err := RowPlanningDates(`{"scheduled_on":42,"deadline_on":"2026-11-02"}`, "", "")
	require.NoError(t, err)
	assert.Empty(t, scheduledOn)
	assert.Equal(t, "2026-11-02", deadlineOn)

	scheduledOn, deadlineOn, err = RowPlanningDates(`{"scheduled_on":"2026-10-29","deadline_on":["x"]}`, "", "")
	require.NoError(t, err)
	assert.Equal(t, "2026-10-29", scheduledOn)
	assert.Empty(t, deadlineOn)
}

func TestRowPlanningDatesFailsOnUnsupportedTZDataVersion(t *testing.T) {
	raw := `{"scheduled_on":{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"},"deadline_on":"2026-11-02"}`
	_, _, err := RowPlanningDates(raw, "", "")
	require.ErrorContains(t, err, "unsupported schedule tzdata version")
}

func TestRowPlanningDatesReportsUnknownScheduleMember(t *testing.T) {
	raw := `{"scheduled_on":{"value":"2026-11-12","timezone":"UTC","tzdata_version":"` + metadata.ScheduleTZDataVersion + `","future":1},"deadline_on":"2026-11-02"}`
	_, _, err := RowPlanningDates(raw, "", "")
	require.ErrorIs(t, err, json.ErrUnknownName)
}

func TestCalendarProjectionUsesEachSavedZoneAndBoundary(t *testing.T) {
	raw := `{"scheduled_on":{"value":"2026-01-01T02:00:00Z","timezone":"America/New_York","tzdata_version":"` + metadata.ScheduleTZDataVersion + `"},"deadline_on":{"value":"2026-01-01","timezone":"Asia/Tokyo","tzdata_version":"` + metadata.ScheduleTZDataVersion + `"}}`
	var boundary time.Time
	query := UISnapshotQuery{View: "today", TimeZone: "America/Los_Angeles", CalendarAt: "2026-01-01T01:00:00Z", CalendarBoundary: &boundary}
	scheduled, deadline, scheduledToday, deadlineToday, included, err := MatchUICalendarView(raw, query, "UTC")
	require.NoError(t, err)
	assert.Equal(t, "2025-12-31", scheduled)
	assert.Equal(t, "2026-01-01", deadline)
	assert.Equal(t, "2025-12-31", scheduledToday)
	assert.Equal(t, "2026-01-01", deadlineToday)
	assert.True(t, included)
	assert.Equal(t, "2026-01-01T05:00:00Z", boundary.UTC().Format(time.RFC3339))
}

func TestCalendarProjectionOmitsOpaqueField(t *testing.T) {
	raw := `{"scheduled_on":{"note":"opaque"},"deadline_on":"2026-01-01"}`
	query := UISnapshotQuery{View: "today", TimeZone: "UTC", CalendarAt: "2026-01-01T01:00:00Z"}
	scheduled, deadline, _, _, included, err := MatchUICalendarView(raw, query, "UTC")
	require.NoError(t, err)
	assert.Empty(t, scheduled)
	assert.Equal(t, "2026-01-01", deadline)
	assert.True(t, included)
}

func TestEmptyScheduleProjectionUsesIssueZoneForRelativeEdits(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	_, _, scheduledToday, deadlineToday, _, _, err := RowPlanningProjection(`{"timezone":"Pacific/Kiritimati"}`, "UTC", "UTC", now)
	require.NoError(t, err)
	assert.Equal(t, "2026-11-03", scheduledToday)
	assert.Equal(t, "2026-11-03", deadlineToday)
}
