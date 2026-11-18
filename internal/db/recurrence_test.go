package db

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	value, err := ComposeRecurrenceIssueMetadata(jsontext.Value(`{"kind":"weekly","timezone":"Not/AZone","scheduled_on":"not-a-date"}`), "2026-05-18", "America/New_York", "")
	require.NoError(t, err)
	assert.JSONEq(t, `{"kind":"weekly","scheduled_on":"2026-05-18[America/New_York]","timezone":"America/New_York"}`, string(value))
}

func TestComposeRecurrenceIssueMetadataDefensivelyRejectsInvalidReservedValue(t *testing.T) {
	_, err := ComposeRecurrenceIssueMetadata(
		jsontext.Value(`{"deadline_on":"not-a-date"}`),
		"2026-05-18",
		"America/New_York",
		"",
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRecurrence)
}

func TestRecurrenceDeadlineKeepsItsZone(t *testing.T) {
	value, err := ComposeRecurrenceIssueMetadata(jsontext.Value(`{"deadline_on":"2026-06-01T09:00"}`), "2026-05-18", "Asia/Tokyo", "America/New_York")
	require.NoError(t, err)
	assert.JSONEq(t, `{"scheduled_on":"2026-05-18[Asia/Tokyo]","timezone":"Asia/Tokyo","deadline_on":"2026-06-01T09:00[Asia/Tokyo]"}`, string(value))

	value, err = ComposeExistingRecurrenceIssueMetadata(jsontext.Value(`{"deadline_on":"2026-06-01T09:00","timezone":"Europe/Paris"}`), "2026-05-18", "Asia/Tokyo", "UTC")
	require.NoError(t, err)
	assert.JSONEq(t, `{"scheduled_on":"2026-05-18[Asia/Tokyo]","timezone":"Asia/Tokyo","deadline_on":"2026-06-01T09:00[Europe/Paris]"}`, string(value))
}
