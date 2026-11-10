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
	patch, err := freezeIssueSchedules(current, map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-11-12T09:00"`)}, "UTC", "")
	require.NoError(t, err)
	require.JSONEq(t, string(original), string(patch["scheduled_on"]))
	patch, err = freezeIssueSchedules(current, map[string]jsontext.Value{"timezone": jsontext.Value(`"America/New_York"`)}, "UTC", "")
	require.NoError(t, err)
	raw, err := db.ApplyMetadataPatch(current, patch)
	require.NoError(t, err)
	_, zone, _, _, err := metadata.ScheduleFieldInstant(string(raw), "scheduled_on", "UTC")
	require.NoError(t, err)
	require.Equal(t, "America/New_York", zone)
}
