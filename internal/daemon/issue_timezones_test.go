package daemon

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

func TestFreezeSchedulePreservesEchoAndRebindsZone(t *testing.T) {
	original, err := metadata.NewScheduleValue("2026-11-12T09:00", "Europe/Paris")
	require.NoError(t, err)
	current := jsontext.Value(`{"scheduled_on":` + string(original) + `}`)
	patch, err := prepareIssueSchedules(current, map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-11-12T09:00"`)}, "UTC", "")
	require.NoError(t, err)
	require.JSONEq(t, string(original), string(patch["scheduled_on"]))
	patch, err = prepareIssueSchedules(current, map[string]jsontext.Value{"timezone": jsontext.Value(`"America/New_York"`)}, "UTC", "")
	require.NoError(t, err)
	raw, err := db.ApplyMetadataPatch(current, patch)
	require.NoError(t, err)
	_, zone, _, _, err := metadata.ScheduleFieldInstant(string(raw), "scheduled_on", "UTC")
	require.NoError(t, err)
	require.Equal(t, "America/New_York", zone)
}

func TestScheduleEditKeepsSavedZone(t *testing.T) {
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		t.Run(field, func(t *testing.T) {
			original, err := metadata.NewScheduleValue("2026-01-01T02:00:00Z", "America/New_York")
			require.NoError(t, err)
			current := jsontext.Value(`{"` + field + `":` + string(original) + `}`)
			patch, err := prepareIssueSchedules(current, map[string]jsontext.Value{field: jsontext.Value(`"2026-01-02T02:00:00Z"`)}, "Europe/Paris", "")
			require.NoError(t, err)
			descriptor, err := metadata.DecodeSavedSchedule(patch[field])
			require.NoError(t, err)
			require.Equal(t, "America/New_York", descriptor.Timezone)
			require.Equal(t, metadata.ScheduleTZDataVersion, descriptor.TZDataVersion)
		})
	}
}

func TestUTCInstantUsesAvailableZone(t *testing.T) {
	for _, tc := range []struct {
		name, issue, recurrence, fallback, want string
	}{
		{"issue zone", `{"timezone":"America/New_York"}`, "", "UTC", "America/New_York"},
		{"recurrence zone", `{}`, "US/Eastern", "UTC", "US/Eastern"},
		{"unusable recurrence zone", `{}`, "Local", "UTC", "UTC"},
		{"unusable default", `{}`, "", "Local", "UTC"},
		{"unusable issue zone", `{"timezone":17}`, "", "Local", "UTC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := prepareIssueSchedules(jsontext.Value(tc.issue), map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-01-01T02:00:00Z"`)}, tc.fallback, tc.recurrence)
			require.NoError(t, err)
			saved, err := metadata.DecodeSavedSchedule(patch["scheduled_on"])
			require.NoError(t, err)
			require.Equal(t, tc.want, saved.Timezone)
		})
	}
}
