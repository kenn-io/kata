package db_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

func TestPrepareIssueSchedules(t *testing.T) {
	for _, tc := range []struct {
		name, current, patch, fallback, recurrence, wantZone string
		wantError, localUnknown                              bool
	}{
		{"replace opaque stored date", `{"$field":{"legacy":"opaque"}}`, `{"$field":"2026-11-13"}`, "America/New_York", "", "America/New_York", false, false},
		{"refuse stored future rules", `{"$field":{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"}}`, `{"$field":"2026-11-13"}`, "UTC", "", "", true, false},
		{"refuse stored future members", `{"$field":{"value":"2026-11-12","timezone":"UTC","tzdata_version":"` + metadata.ScheduleTZDataVersion + `","future":true}}`, `{"$field":"2026-11-13"}`, "UTC", "", "", true, false},
		{"UTC", "{}", `{"timezone":"UTC"}`, "Local", "", "UTC", false, false},
		{"Etc UTC", "{}", `{"timezone":"Etc/UTC"}`, "Local", "", "Etc/UTC", false, false},
		{"New York", "{}", `{"timezone":"America/New_York"}`, "Local", "", "America/New_York", false, false},
		{"alias geographic", "{}", `{"timezone":"Asia/Calcutta"}`, "Local", "", "Asia/Calcutta", false, false},
		{"deep geographic", "{}", `{"timezone":"America/Argentina/Buenos_Aires"}`, "Local", "", "America/Argentina/Buenos_Aires", false, false},
		{"Local", "{}", `{"timezone":"Local"}`, "Local", "", "", true, false},
		{"EST", "{}", `{"timezone":"EST"}`, "Local", "", "", true, false},
		{"alias outside geographic", "{}", `{"timezone":"US/Eastern"}`, "Local", "", "", true, false},
		{"fixed offset", "{}", `{"timezone":"Etc/GMT+5"}`, "Local", "", "", true, false},
		{"wrong case", "{}", `{"timezone":"america/New_York"}`, "Local", "", "", true, false},
		{"empty zone", "{}", `{"timezone":""}`, "Local", "", "", true, false},
		{"civil unusable default", "{}", `{"$field":"2026-11-12T10:30"}`, "Local", "", "", true, true},
		{"portable override", "{}", `{"$field":"2026-11-12","timezone":"UTC"}`, "Local", "", "UTC", false, false},
		{"UTC unusable default", "{}", `{"$field":"2026-11-12T10:30:00Z"}`, "Local", "", "UTC", false, true},
		{"omitted default", "{}", `{"$field":"2026-11-12"}`, "UTC", "", "UTC", false, false},
		{"existing alias", `{"timezone":"US/Eastern"}`, `{"$field":"2026-11-12"}`, "UTC", "", "US/Eastern", false, false},
		{"default alias", "{}", `{"$field":"2026-11-12"}`, "US/Eastern", "", "US/Eastern", false, false},
		{"legacy echo", `{"timezone":"Local","scheduled_on":"2026-11-12"}`, `{"timezone":"Local"}`, "Local", "", "", false, false},
		{"unrelated invalid zone", `{"timezone":17}`, `{"topic":"x"}`, "Local", "", "", false, false},
		{"restored clear", `{"timezone":17,"$field":"2026-11-12"}`, `{"$field":null}`, "Local", "", "", false, false},
		{"restored no-op", `{"timezone":17,"$field":"2026-11-12"}`, `{"$field":"2026-11-12"}`, "Local", "", "", false, false},
		{"legacy no-op with known zone", `{"timezone":"America/New_York","$field":"2026-11-12"}`, `{"$field":"2026-11-12"}`, "UTC", "", "America/New_York", false, false},
		{"legacy no-op with empty issue zone", `{"timezone":"","$field":"2026-11-12T09:00"}`, `{"$field":"2026-11-12T09:00"}`, "", "", "UTC", false, false},
		{"legacy no-op with daemon zone", `{"$field":"2026-11-12T09:00"}`, `{"$field":"2026-11-12T09:00"}`, "America/New_York", "", "America/New_York", false, false},
		{"legacy no-op with implicit UTC", `{"$field":"2026-11-12T09:00"}`, `{"$field":"2026-11-12T09:00"}`, "", "", "UTC", false, false},
		{"legacy no-op with recurrence zone", `{"$field":"2026-11-12"}`, `{"$field":"2026-11-12"}`, "UTC", "US/Eastern", "US/Eastern", false, false},
		{"deadline uses existing alias", `{"timezone":"US/Eastern","scheduled_on":"2026-11-12"}`, `{"deadline_on":"2026-11-14"}`, "Local", "US/Eastern", "US/Eastern", false, false},
		{"legacy no-op with linked recurrence alias", `{"timezone":"US/Eastern","scheduled_on":"2026-11-12"}`, `{"scheduled_on":"2026-11-12"}`, "UTC", "US/Eastern", "US/Eastern", false, false},
		{"clear recurrence zone", `{"timezone":"US/Eastern","scheduled_on":"2026-11-12"}`, `{"timezone":null,"scheduled_on":"2026-11-15"}`, "Local", "US/Eastern", "US/Eastern", false, false},
		{"replace opaque", `{"timezone":"America/New_York","$field":{"legacy":"opaque"}}`, `{"$field":"2026-11-12"}`, "UTC", "", "America/New_York", false, false},
		{"replace unknown rules and zone", `{"$field":{"value":"2026-11-01","timezone":"UTC","tzdata_version":"future"}}`, `{"$field":"2026-11-12","timezone":"Europe/Paris"}`, "UTC", "", "", true, false},
		{"replace unknown member and zone", `{"$field":{"value":"2026-11-01","timezone":"UTC","tzdata_version":"` + metadata.ScheduleTZDataVersion + `","future":1}}`, `{"$field":"2026-11-12","timezone":"Europe/Paris"}`, "UTC", "", "", true, false},
		{"restored UTC", `{"timezone":17}`, `{"$field":"2026-11-12T10:30:00Z"}`, "Local", "", "UTC", false, false},
		{"restored civil", `{"timezone":17}`, `{"$field":"2026-11-12"}`, "UTC", "", "", true, false},
		{"remove zone", `{"timezone":"UTC","$field":"2026-11-12"}`, `{"timezone":null}`, "Local", "", "", true, true},
		{"clear both", `{"timezone":"Local","scheduled_on":"2026-11-12"}`, `{"timezone":null,"scheduled_on":null}`, "Local", "", "", false, false},
		{"remove instant zone", `{"timezone":"Local","deadline_on":"2026-11-12T10:30:00Z"}`, `{"timezone":null}`, "Local", "", "", false, false},
		{"remove empty zone", `{"timezone":"","scheduled_on":"2026-11-12"}`, `{"timezone":null}`, "Local", "", "", false, false},
		{"issue zone for instant", `{"timezone":"America/New_York"}`, `{"$field":"2026-01-01T02:00:00Z"}`, "UTC", "", "America/New_York", false, false},
		{"recurrence zone for instant", "{}", `{"$field":"2026-01-01T02:00:00Z"}`, "UTC", "US/Eastern", "US/Eastern", false, false},
		{"unusable recurrence zone for instant", "{}", `{"$field":"2026-01-01T02:00:00Z"}`, "UTC", "Mars/Olympus_Mons", "UTC", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.localUnknown {
				previous := time.Local
				time.Local = time.FixedZone("Mars/Olympus_Mons", 0)
				t.Cleanup(func() { time.Local = previous })
			}
			fields := []string{"scheduled_on"}
			if strings.Contains(tc.patch, `"deadline_on"`) {
				fields = []string{"deadline_on"}
			}
			if strings.Contains(tc.current+tc.patch, "$field") && tc.recurrence == "" {
				fields = append(fields, "deadline_on")
			}
			for _, field := range fields {
				var patch map[string]jsontext.Value
				require.NoError(t, json.Unmarshal([]byte(strings.ReplaceAll(tc.patch, "$field", field)), &patch))
				out, err := db.PrepareIssueSchedules(jsontext.Value(strings.ReplaceAll(tc.current, "$field", field)), patch, tc.fallback, tc.recurrence)
				if tc.wantError {
					require.ErrorIs(t, err, metadata.ErrInvalidValue)
					var current map[string]jsontext.Value
					require.NoError(t, json.Unmarshal([]byte(strings.ReplaceAll(tc.current, "$field", field)), &current))
					if raw := current[field]; len(raw) > 0 && raw[0] == '{' {
						_, cause := metadata.DecodeSavedSchedule(raw)
						require.ErrorContains(t, err, field+": "+cause.Error())
					}
					continue
				}
				require.NoError(t, err)
				if tc.wantZone != "" && len(out[field]) == 0 {
					require.JSONEq(t, `"`+tc.wantZone+`"`, string(out["timezone"]))
				} else if tc.wantZone != "" {
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
