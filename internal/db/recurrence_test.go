package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"os/exec"
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
		jsontext.Value(`{"kind":"weekly","timezone":"Not/AZone","scheduled_on":"not-a-date"}`),
		"2026-05-18",
		"America/New_York",
	)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"kind":"weekly",
		"scheduled_on":{"value":"2026-05-18","timezone":"America/New_York","tzdata_version":"sha256:b2d18a7c8fa8142097a48c99609fb3c92db5ee98bc740294e57eab8ae9f94779"},
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

func TestRecurrenceMaterializesSavedRules(t *testing.T) {
	value, err := ComposeRecurrenceIssueMetadata(jsontext.Value(`{"deadline_on":"2026-05-19T09:00"}`), "2026-05-18", "America/New_York")
	require.NoError(t, err)
	var values map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(value, &values))
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		require.Equal(t, byte('{'), values[field][0])
		_, zone, _, present, err := metadata.ScheduleFieldInstant(string(value), field, "UTC")
		require.NoError(t, err)
		require.True(t, present)
		require.Equal(t, "America/New_York", zone)
	}
}

func TestRecurrenceLocalMaterializesInNamedZone(t *testing.T) {
	if os.Getenv("KATA_TEST_LOCAL_RECURRENCE") == "1" {
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
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestRecurrenceLocalMaterializesInNamedZone$")
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
