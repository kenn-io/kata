package db_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

func TestPrepareIssueSchedules(t *testing.T) {
	for _, tc := range []struct {
		name, current, patch, fallback, recurrence, wantZone string
		wantError                                            bool
	}{
		{"UTC", "{}", `{"timezone":"UTC"}`, "Local", "", "", false},
		{"Etc UTC", "{}", `{"timezone":"Etc/UTC"}`, "Local", "", "", false},
		{"New York", "{}", `{"timezone":"America/New_York"}`, "Local", "", "", false},
		{"alias geographic", "{}", `{"timezone":"Asia/Calcutta"}`, "Local", "", "", false},
		{"deep geographic", "{}", `{"timezone":"America/Argentina/Buenos_Aires"}`, "Local", "", "", false},
		{"Local", "{}", `{"timezone":"Local"}`, "Local", "", "", true},
		{"EST", "{}", `{"timezone":"EST"}`, "Local", "", "", true},
		{"alias outside geographic", "{}", `{"timezone":"US/Eastern"}`, "Local", "", "", true},
		{"fixed offset", "{}", `{"timezone":"Etc/GMT+5"}`, "Local", "", "", true},
		{"wrong case", "{}", `{"timezone":"america/New_York"}`, "Local", "", "", true},
		{"empty zone", "{}", `{"timezone":""}`, "Local", "", "", true},
		{"civil unusable default", "{}", `{"$field":"2026-11-12T10:30"}`, "Local", "", "", true},
		{"portable override", "{}", `{"$field":"2026-11-12","timezone":"UTC"}`, "Local", "", "UTC", false},
		{"UTC unusable default", "{}", `{"$field":"2026-11-12T10:30:00Z"}`, "Local", "", "UTC", false},
		{"omitted default", "{}", `{"$field":"2026-11-12"}`, "UTC", "", "UTC", false},
		{"legacy echo", `{"timezone":"Local","scheduled_on":"2026-11-12"}`, `{"timezone":"Local"}`, "Local", "", "", false},
		{"legacy schedule edit", `{"timezone":"Local"}`, `{"$field":"2026-11-12"}`, "UTC", "", "", true},
		{"unrelated invalid zone", `{"timezone":17}`, `{"topic":"x"}`, "Local", "", "", false},
		{"restored clear", `{"timezone":17,"$field":"2026-11-12"}`, `{"$field":null}`, "Local", "", "", false},
		{"restored no-op", `{"timezone":17,"$field":"2026-11-12"}`, `{"$field":"2026-11-12"}`, "Local", "", "", false},
		{"legacy no-op with known zone", `{"timezone":"America/New_York","$field":"2026-11-12"}`, `{"$field":"2026-11-12"}`, "UTC", "", "America/New_York", false},
		{"legacy no-op with empty issue zone", `{"timezone":"","$field":"2026-11-12T09:00"}`, `{"$field":"2026-11-12T09:00"}`, "", "", "UTC", false},
		{"legacy no-op with implicit UTC", `{"$field":"2026-11-12T09:00"}`, `{"$field":"2026-11-12T09:00"}`, "", "", "UTC", false},
		{"legacy no-op with daemon zone", `{"$field":"2026-11-12T09:00"}`, `{"$field":"2026-11-12T09:00"}`, "America/New_York", "", "America/New_York", false},
		{"legacy no-op with recurrence zone", `{"$field":"2026-11-12"}`, `{"$field":"2026-11-12"}`, "UTC", "US/Eastern", "US/Eastern", false},
		{"legacy no-op with linked recurrence alias", `{"timezone":"US/Eastern","scheduled_on":"2026-11-12"}`, `{"scheduled_on":"2026-11-12"}`, "UTC", "US/Eastern", "US/Eastern", false},
		{"deadline needs portable default", `{"timezone":"US/Eastern","scheduled_on":"2026-11-12"}`, `{"deadline_on":"2026-11-14"}`, "Local", "US/Eastern", "", true},
		{"clear recurrence zone", `{"timezone":"US/Eastern","scheduled_on":"2026-11-12"}`, `{"timezone":null,"scheduled_on":"2026-11-15"}`, "Local", "US/Eastern", "US/Eastern", false},
		{"restored UTC", `{"timezone":17}`, `{"$field":"2026-11-12T10:30:00Z"}`, "Local", "", "UTC", false},
		{"restored civil", `{"timezone":17}`, `{"$field":"2026-11-12"}`, "UTC", "", "", true},
		{"remove zone", `{"timezone":"UTC","$field":"2026-11-12"}`, `{"timezone":null}`, "Local", "", "", true},
		{"clear both", `{"timezone":"Local","scheduled_on":"2026-11-12"}`, `{"timezone":null,"scheduled_on":null}`, "Local", "", "", false},
		{"remove instant zone", `{"timezone":"Local","deadline_on":"2026-11-12T10:30:00Z"}`, `{"timezone":null}`, "Local", "", "", false},
		{"remove empty zone", `{"timezone":"","scheduled_on":"2026-11-12"}`, `{"timezone":null}`, "Local", "", "", false},
		{"issue zone for instant", `{"timezone":"America/New_York"}`, `{"$field":"2026-01-01T02:00:00Z"}`, "UTC", "", "America/New_York", false},
		{"recurrence zone for instant", "{}", `{"$field":"2026-01-01T02:00:00Z"}`, "UTC", "US/Eastern", "US/Eastern", false},
		{"unusable recurrence zone for instant", "{}", `{"$field":"2026-01-01T02:00:00Z"}`, "UTC", "Local", "UTC", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := []string{"scheduled_on"}
			if strings.Contains(tc.current+tc.patch, "$field") && tc.recurrence == "" {
				fields = append(fields, "deadline_on")
			}
			for _, field := range fields {
				var patch map[string]jsontext.Value
				require.NoError(t, json.Unmarshal([]byte(strings.ReplaceAll(tc.patch, "$field", field)), &patch))
				out, err := db.PrepareIssueSchedules(jsontext.Value(strings.ReplaceAll(tc.current, "$field", field)), patch, tc.fallback, tc.recurrence)
				if tc.wantError {
					require.Error(t, err)
					continue
				}
				require.NoError(t, err)
				if tc.wantZone != "" {
					saved, err := metadata.DecodeSavedSchedule(out[field])
					require.NoError(t, err)
					require.NotNil(t, saved)
					require.Equal(t, tc.wantZone, saved.Timezone)
					require.Equal(t, metadata.ScheduleTZDataVersion, saved.TZDataVersion)
				}
			}
		})
	}
}

func TestPrepareIssueSchedulesPreservesAndRebinds(t *testing.T) {
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		t.Run(field, func(t *testing.T) {
			original, err := metadata.NewScheduleValue("2026-01-01T02:00:00Z", "America/New_York")
			require.NoError(t, err)
			current := jsontext.Value(`{"` + field + `":` + string(original) + `}`)
			echo, err := db.PrepareIssueSchedules(current, map[string]jsontext.Value{field: jsontext.Value(`"2026-01-01T02:00:00Z"`)}, "Europe/Paris", "")
			require.NoError(t, err)
			require.JSONEq(t, string(original), string(echo[field]))
			edited, err := db.PrepareIssueSchedules(current, map[string]jsontext.Value{field: jsontext.Value(`"2026-01-02T02:00:00Z"`)}, "Europe/Paris", "")
			require.NoError(t, err)
			saved, err := metadata.DecodeSavedSchedule(edited[field])
			require.NoError(t, err)
			require.Equal(t, "America/New_York", saved.Timezone)
			require.Equal(t, metadata.ScheduleTZDataVersion, saved.TZDataVersion)
			rebound, err := db.PrepareIssueSchedules(current, map[string]jsontext.Value{"timezone": jsontext.Value(`"Europe/Paris"`)}, "UTC", "")
			require.NoError(t, err)
			saved, err = metadata.DecodeSavedSchedule(rebound[field])
			require.NoError(t, err)
			require.Equal(t, "Europe/Paris", saved.Timezone)
		})
	}
}

func TestPrepareIssueSchedulesPreservesSuppliedObject(t *testing.T) {
	saved, err := metadata.NewScheduleValue("2026-01-01T02:00:00Z", "America/New_York")
	require.NoError(t, err)
	for _, current := range []jsontext.Value{jsontext.Value(`{}`), jsontext.Value(`{"scheduled_on":"2026-01-01T02:00:00Z"}`)} {
		patch := map[string]jsontext.Value{"scheduled_on": saved, "timezone": jsontext.Value(`"Europe/Paris"`)}
		out, err := db.PrepareIssueSchedules(current, patch, "UTC", "")
		require.NoError(t, err)
		require.JSONEq(t, string(saved), string(out["scheduled_on"]))
	}
	other, err := metadata.NewScheduleValue("2026-01-01T02:00:00Z", "Europe/Paris")
	require.NoError(t, err)
	current := jsontext.Value(`{"scheduled_on":` + string(saved) + `}`)
	out, err := db.PrepareIssueSchedules(current, map[string]jsontext.Value{"scheduled_on": other}, "UTC", "")
	require.NoError(t, err)
	require.JSONEq(t, string(other), string(out["scheduled_on"]))
	_, err = db.PrepareIssueSchedules(jsontext.Value(`{}`), map[string]jsontext.Value{"scheduled_on": jsontext.Value(`{"value":"2026-01-01","timezone":"UTC","tzdata_version":"unknown"}`)}, "UTC", "")
	require.ErrorContains(t, err, "unsupported schedule tzdata version")
}
