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
	for _, tc := range []struct {
		name        string
		compose     func(jsontext.Value, string, string) (jsontext.Value, error)
		input, want string
	}{
		{"template", ComposeRecurrenceIssueMetadata, `{"kind":"weekly","timezone":"Not/AZone","scheduled_on":"not-a-date"}`, `{"kind":"weekly","scheduled_on":"2026-05-18[America/New_York]","timezone":"America/New_York"}`},
		{"existing deadline retains its zone", ComposeExistingRecurrenceIssueMetadata, `{"deadline_on":"2026-05-20[Asia/Tokyo]"}`, `{"scheduled_on":"2026-05-18[America/New_York]","deadline_on":"2026-05-20[Asia/Tokyo]","timezone":"America/New_York"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := tc.compose(jsontext.Value(tc.input), "2026-05-18", "America/New_York")
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(value))
		})
	}
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
