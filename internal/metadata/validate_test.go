package metadata

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 26-char ULIDs from the Crockford alphabet.
const goodULID1 = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
const goodULID2 = "01ARZ3NDEKTSV4RRFFQ69G5FAW"

// TestValidateCreateValue_RejectsNullAndEmpty pins the create-time rule that a
// null or empty value is a caller error (nothing to clear at creation), and
// that the rejection wraps ErrInvalidValue so handlers map it to a 400.
func TestValidateCreateValue_RejectsNullAndEmpty(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(``)} {
		err := ValidateCreateValue(IssueRegistry, "scheduled_on", raw)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidValue)
		assert.Contains(t, err.Error(), "null values are not allowed at creation")
	}
}

// TestValidateCreateValue_DelegatesToValidate pins that non-null values flow
// through Validate: reserved keys are type-checked, unknown keys pass opaquely.
func TestValidateCreateValue_DelegatesToValidate(t *testing.T) {
	assert.NoError(t, ValidateCreateValue(IssueRegistry, "scheduled_on", json.RawMessage(`"2026-05-20"`)))
	err := ValidateCreateValue(IssueRegistry, "scheduled_on", json.RawMessage(`"not-a-date"`))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue)
	assert.NoError(t, ValidateCreateValue(IssueRegistry, "weird_consumer_field", json.RawMessage(`"hi"`)))
}

func TestValidateScheduleValues(t *testing.T) {
	for _, key := range []string{"scheduled_on", "deadline_on"} {
		t.Run(key, func(t *testing.T) {
			assert.NoError(t, Validate(IssueRegistry, key, json.RawMessage(`"2026-05-20"`)))
			assert.NoError(t, Validate(IssueRegistry, key, json.RawMessage(`"2026-05-20T15:30"`)))
			assert.NoError(t, Validate(IssueRegistry, key, json.RawMessage(`"2026-05-20T15:30:45"`)))
			assert.NoError(t, Validate(IssueRegistry, key, json.RawMessage(`"2026-05-20T22:30:00Z"`)))
			assert.Error(t, Validate(IssueRegistry, key, json.RawMessage(`"2026-05-20T15:30:00-07:00"`)))
			assert.Error(t, Validate(IssueRegistry, key, json.RawMessage(`"not-a-date"`)))
			assert.Error(t, Validate(IssueRegistry, key, json.RawMessage(`"2026-13-01"`)))
			assert.Error(t, Validate(IssueRegistry, key, json.RawMessage(`123`)))
		})
	}
}

func TestValidateBool(t *testing.T) {
	assert.NoError(t, Validate(IssueRegistry, "someday", json.RawMessage(`true`)))
	assert.NoError(t, Validate(IssueRegistry, "someday", json.RawMessage(`false`)))
	assert.Error(t, Validate(IssueRegistry, "someday", json.RawMessage(`"true"`)))
}

func TestValidateChecklist(t *testing.T) {
	good := json.RawMessage(fmt.Sprintf(`[
		{"id":%q,"text":"draft","done":false},
		{"id":%q,"text":"ship","done":true}
	]`, goodULID1, goodULID2))
	assert.NoError(t, Validate(IssueRegistry, "checklist", good))

	badULID := json.RawMessage(`[{"id":"oops","text":"t","done":false}]`)
	assert.Error(t, Validate(IssueRegistry, "checklist", badULID))

	missingText := json.RawMessage(fmt.Sprintf(`[{"id":%q,"done":false}]`, goodULID1))
	assert.Error(t, Validate(IssueRegistry, "checklist", missingText))
}

func TestValidateChecklist_MissingDoneRejected(t *testing.T) {
	// Item has id and text but no "done" field — must be rejected.
	noDone := json.RawMessage(fmt.Sprintf(`[{"id":%q,"text":"x"}]`, goodULID1))
	err := Validate(IssueRegistry, "checklist", noDone)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidValue)
	assert.Contains(t, err.Error(), "done required")
}

func TestValidateTimezone(t *testing.T) {
	assert.NoError(t, Validate(IssueRegistry, "timezone", json.RawMessage(`"America/New_York"`)))
	assert.Error(t, Validate(IssueRegistry, "timezone", json.RawMessage(`"NotAReal/Zone"`)))
}

// TestValidate_UnknownKeysAcceptedOpaquely covers the contract that anything
// not in the registry is accepted as a pass-through value: the daemon does
// not enforce a closed metadata schema. Consumers can write their own keys
// (UI hints, application-specific data) without coordinating a daemon
// release.
func TestValidate_UnknownKeysAcceptedOpaquely(t *testing.T) {
	assert.NoError(t, Validate(IssueRegistry, "definitely_not_a_key", json.RawMessage(`null`)))
	assert.NoError(t, Validate(IssueRegistry, "weird_consumer_field", json.RawMessage(`"hi"`)))
	assert.NoError(t, Validate(IssueRegistry, "extra_bag", json.RawMessage(`{"a":1,"b":[true]}`)))
	assert.NoError(t, Validate(ProjectRegistry, "sidebar_order", json.RawMessage(`"first"`)),
		"a previously reserved key now behaves like any unknown key")
}

func TestValidateNullClears(t *testing.T) {
	assert.NoError(t, Validate(IssueRegistry, "scheduled_on", json.RawMessage(`null`)))
	assert.NoError(t, Validate(IssueRegistry, "checklist", json.RawMessage(`null`)))
}

// TestValidate_AllErrorsWrapErrInvalidValue locks in the invariant that every
// reserved-key validator failure is detectable via errors.Is(err,
// ErrInvalidValue). Handlers translate this to a 400 response; if a validator
// returns a plain error, invalid client input falls through to a 500.
func TestValidate_AllErrorsWrapErrInvalidValue(t *testing.T) {
	cases := []struct {
		name     string
		registry map[string]Entry
		key      string
		raw      json.RawMessage
	}{
		{"date_wrong_type", IssueRegistry, "scheduled_on", json.RawMessage(`123`)},
		{"date_malformed", IssueRegistry, "scheduled_on", json.RawMessage(`"not-a-date"`)},
		{"bool_wrong_type", IssueRegistry, "someday", json.RawMessage(`"yes"`)},
		{"timezone_wrong_type", IssueRegistry, "timezone", json.RawMessage(`123`)},
		{"timezone_bogus", IssueRegistry, "timezone", json.RawMessage(`"Not/Real"`)},
		{"project_string_wrong_type", ProjectRegistry, "area", json.RawMessage(`123`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.registry, c.key, c.raw)
			assert.ErrorIs(t, err, ErrInvalidValue,
				"validator must wrap ErrInvalidValue so handlers map to 400")
		})
	}
}
