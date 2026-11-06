package api

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// The public run remains flat and byte-identical when no verdict was evaluated.
func TestCronRunFlatJSON(t *testing.T) {
	row := db.CronRun{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}, UpdatedAt: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	want, err := json.Marshal(row)
	require.NoError(t, err)
	public := CronRunFrom(row)
	got, err := json.Marshal(public)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
	public.Staleness = cron.Staleness(row.Status, row.UpdatedAt, 0, row.UpdatedAt)
	got, err = json.Marshal(public)
	require.NoError(t, err)
	var flat map[string]any
	require.NoError(t, json.Unmarshal(got, &flat))
	require.Equal(t, row.UID, flat["uid"])
	require.Contains(t, flat, "staleness")
	require.NotContains(t, flat, "CronRun")
}
