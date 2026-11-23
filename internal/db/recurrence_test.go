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
	value, err := ComposeRecurrenceIssueMetadata(
		jsontext.Value(`{"kind":"weekly","timezone":"Not/AZone","scheduled_on":"not-a-date"}`),
		"2026-05-18",
		"America/New_York",
	)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"kind":"weekly",
		"scheduled_on":"2026-05-18",
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

func TestComposeRecurrenceMetadataPreservesLegacyAttention(t *testing.T) {
	value, err := ComposeExistingRecurrenceIssueMetadata(jsontext.Value(`{"work.attention":"working"}`), "2026-11-23", "UTC")
	require.NoError(t, err)
	assert.JSONEq(t, `{"work.attention":"working","scheduled_on":"2026-11-23","timezone":"UTC"}`, string(value))
	for _, tc := range []struct{ input, want string }{
		{`{"work.attention":"working","kind":"weekly"}`, `{"kind":"weekly","scheduled_on":"2026-11-23","timezone":"UTC"}`},
		{`{"work.attention":"stuck"}`, `{"work.attention":"stuck","scheduled_on":"2026-11-23","timezone":"UTC"}`},
	} {
		value, err := ComposeRecurrenceIssueMetadata(jsontext.Value(tc.input), "2026-11-23", "UTC")
		require.NoError(t, err)
		assert.JSONEq(t, tc.want, string(value))
	}
}

func TestValidateRecurrenceTemplatePatchSkipsOnlySavedLegacyAttention(t *testing.T) {
	assert.ErrorIs(t, ValidateRecurrenceTemplatePatch("Review", jsontext.Value(`{"work.attention":"working","someday":"yes"}`), nil), ErrInvalidRecurrence)
}
