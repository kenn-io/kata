package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveDateInput(t *testing.T) {
	now := time.Date(2026, time.November, 2, 9, 30, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, in, want string
	}{
		{"blank clears", "", ""},
		{"spaces clear", "   ", ""},
		{"calendar date", "2026-11-15", "2026-11-15"},
		{"local minute", "2026-11-15T09:00", "2026-11-15T09:00"},
		{"local second", "2026-11-15T09:00:30", "2026-11-15T09:00:30"},
		{"utc instant", "2026-11-15T09:00:00Z", "2026-11-15T09:00:00Z"},
		{"today", "today", "2026-11-02"},
		{"tomorrow", "tomorrow", "2026-11-03"},
		{"days", "+3d", "2026-11-05"},
		{"weeks", "+2w", "2026-11-16"},
		{"zero days", "+0d", "2026-11-02"},
		{"case and padding", "  Tomorrow ", "2026-11-03"},
		{"upper unit", "+1W", "2026-11-09"},
		{"month rollover", "+30d", "2026-12-02"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveDateInput(tt.in, now)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveDateInputRefusesWhatTheDaemonRefuses(t *testing.T) {
	now := time.Date(2026, time.November, 2, 9, 30, 0, 0, time.UTC)
	for _, in := range []string{
		"monday", "next week", "+3", "+d", "-3d", "+3m", "+10000d", "2026-13-01", "2026-11-15T09:00+02:00", "11/15/2026",
	} {
		t.Run(in, func(t *testing.T) {
			_, err := resolveDateInput(in, now)
			require.Error(t, err)
			assert.Equal(t, "must be "+dateInputHint, err.Error())
		})
	}
}

func TestResolveDateInputCountsFromTheLocalDay(t *testing.T) {
	// 23:30 on 1 November in UTC-8 is already 2 November in UTC.
	pacific := time.FixedZone("PST", -8*60*60)
	now := time.Date(2026, time.November, 1, 23, 30, 0, 0, pacific)
	require.Equal(t, "2026-11-02", now.UTC().Format(time.DateOnly))

	got, err := resolveDateInput("today", now)
	require.NoError(t, err)
	assert.Equal(t, "2026-11-01", got)

	got, err = resolveDateInput("+1d", now)
	require.NoError(t, err)
	assert.Equal(t, "2026-11-02", got)

	assert.Equal(t, "2026-11-01 Sun", datePreview("today", now))
	assert.Equal(t, "2026-11-02 Mon", datePreview("tomorrow", now))

	east := time.FixedZone("NZDT", 13*60*60)
	now = time.Date(2026, time.November, 2, 1, 0, 0, 0, east)
	require.Equal(t, "2026-11-01", now.UTC().Format(time.DateOnly))
	assert.Equal(t, "2026-11-02 Mon", datePreview("today", now))
}

func TestDatePreview(t *testing.T) {
	now := time.Date(2026, time.November, 2, 9, 30, 0, 0, time.UTC)
	assert.Equal(t, "2026-11-05 Thu", datePreview("+3d", now))
	assert.Equal(t, "2026-11-15 Sun", datePreview("2026-11-15", now))
	assert.Equal(t, "2026-11-15 Sun 09:00", datePreview("2026-11-15T09:00", now))
	assert.Equal(t, "2026-11-15T09:00:00Z", datePreview("2026-11-15T09:00:00Z", now))
	assert.Equal(t, "no date", datePreview("", now))
	assert.Empty(t, datePreview("monday", now))
}
