package db

import (
	"encoding/json/jsontext"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/metadata"
)

func TestValidateRecurrenceTemplateValidatesReservedMetadata(t *testing.T) {
	err := ValidateRecurrenceTemplate("Review", jsontext.Value(`{"timezone":"Not/AZone"}`))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRecurrence)
	assert.Contains(t, err.Error(), `template_metadata "timezone"`)

	assert.NoError(t, ValidateRecurrenceTemplate(
		"Review",
		jsontext.Value(`{"timezone":"America/New_York","custom":{"enabled":true}}`),
	))
}

func TestComposeRecurrenceIssueMetadataStampsScheduleTimezone(t *testing.T) {
	value, err := ComposeRecurrenceIssueMetadata(
		jsontext.Value(`{"kind":"weekly","timezone":"Not/AZone","scheduled_on":"not-a-date","deadline_on":"2026-05-19T09:00"}`),
		"2026-05-18",
		"America/New_York",
	)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"kind":"weekly",
		"scheduled_on":{"value":"2026-05-18","timezone":"America/New_York","tzdata_version":"`+metadata.ScheduleTZDataVersion+`"},
		"deadline_on":{"value":"2026-05-19T09:00","timezone":"America/New_York","tzdata_version":"`+metadata.ScheduleTZDataVersion+`"},
		"timezone":"America/New_York"
	}`, string(value))
}

func TestComposeRecurrenceIssueMetadataDefensivelyRejectsInvalidReservedValue(t *testing.T) {
	_, err := ComposeRecurrenceIssueMetadata(
		jsontext.Value(`{"deadline_on":"not-a-date"}`),
		"2026-05-18",
		"America/New_York",
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRecurrence)
}

func TestRecurrenceLocalMaterializesInNamedZone(t *testing.T) {
	if os.Getenv("KATA_TEST_LOCAL_RECURRENCE") == "1" {
		if runtime.GOOS == "windows" {
			location, err := time.LoadLocation("America/New_York")
			require.NoError(t, err)
			time.Local = location
		}
		value, err := ComposeRecurrenceIssueMetadata(jsontext.Value(`{"deadline_on":"2026-05-19T09:00"}`), "2026-05-18", "Local")
		require.NoError(t, err)
		for _, field := range []string{"scheduled_on", "deadline_on"} {
			_, zone, _, present, err := metadata.ScheduleFieldInstant(string(value), field, "UTC")
			require.NoError(t, err)
			require.True(t, present)
			require.Equal(t, "America/New_York", zone)
		}
		return
	}
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestRecurrenceLocalMaterializesInNamedZone$") // #nosec G204 -- re-execute this test binary with an isolated timezone.
	cmd.Env = append(os.Environ(), "KATA_TEST_LOCAL_RECURRENCE=1", "TZ=America/New_York")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestRecurrenceLocalRejectsDifferentRules(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("fixture", -8*60*60)
	t.Cleanup(func() { time.Local = previous })
	saved, err := metadata.NewScheduleValue("2026-05-19T09:00", "America/New_York")
	require.NoError(t, err)
	err = compareLocalSchedule("2026-05-19T09:00", saved)
	require.ErrorContains(t, err, "Local timezone rules differ")
}

func TestAttachRecurrencePreservesDeadlineZone(t *testing.T) {
	for _, tc := range []struct{ name, raw, defaultZone string }{
		{"issue zone", `{"timezone":"America/New_York","deadline_on":"2026-05-19T09:00"}`, "UTC"},
		{"daemon zone", `{"deadline_on":"2026-05-19T09:00"}`, "America/New_York"},
		{"empty issue zone", `{"timezone":"","deadline_on":"2026-05-19T09:00"}`, "America/New_York"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, before, _, err := metadata.ScheduleFieldInstant(tc.raw, "deadline_on", tc.defaultZone)
			require.NoError(t, err)
			got, err := ComposeExistingRecurrenceIssueMetadata(jsontext.Value(tc.raw), "2026-05-18", "Europe/London", tc.defaultZone)
			require.NoError(t, err)
			_, zone, after, _, err := metadata.ScheduleFieldInstant(string(got), "deadline_on", "")
			require.NoError(t, err)
			assert.Equal(t, "America/New_York", zone)
			assert.Equal(t, before, after)
		})
	}
}

func TestRecurrenceLocalRejectsChangedDeadlineCalendarDate(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("America/Inuvik", -6*60*60)
	t.Cleanup(func() { time.Local = previous })

	const deadline = `{"deadline_on":"2026-11-12T06:30:00Z","timezone":"Local"}`
	_, err := ComposeRecurrenceIssueMetadata(jsontext.Value(deadline), "2026-07-01", "Local")
	require.ErrorContains(t, err, "Local timezone rules differ")
}
