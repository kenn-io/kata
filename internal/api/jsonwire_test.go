package api_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
)

// TestJSONRawObjectRoundTripsVerbatim pins that the opaque raw-object wire
// type keeps json.RawMessage's exact behavior: bytes in, same bytes out, no
// base64 and no re-encoding. A defined type over []byte that forgot its own
// MarshalJSON would silently base64-encode every recurrence template
// metadata blob.
func TestJSONRawObjectRoundTripsVerbatim(t *testing.T) {
	const stored = `{"area":"example-workspace","depth":2}`

	raw, err := json.Marshal(api.JSONRawObject(stored))
	require.NoError(t, err)
	assert.JSONEq(t, stored, string(raw))

	var back api.JSONRawObject
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.JSONEq(t, stored, string(back))

	empty, err := json.Marshal(api.JSONRawObject(nil))
	require.NoError(t, err)
	assert.Equal(t, "null", string(empty))
}

// TestJSONNullableRawObjectRoundTripsVerbatim pins the same behavior for the
// nullable sibling used by the recurrence template patch body.
func TestJSONNullableRawObjectRoundTripsVerbatim(t *testing.T) {
	const stored = `{"area":"example-workspace"}`

	raw, err := json.Marshal(api.JSONNullableRawObject(stored))
	require.NoError(t, err)
	assert.JSONEq(t, stored, string(raw))

	var back api.JSONNullableRawObject
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.JSONEq(t, stored, string(back))
}

// TestRecurrenceTemplateUpdateInputPreservesNullAsAbsent pins the patch
// handler's compatibility contract: both an absent metadata key and an
// explicit null mean "no change", while an object remains available as raw
// bytes. encoding/json intentionally clears pointer fields for null without
// calling the pointed-to type's UnmarshalJSON method.
func TestRecurrenceTemplateUpdateInputPreservesNullAsAbsent(t *testing.T) {
	var absent api.RecurrenceTemplateUpdateInput
	require.NoError(t, json.Unmarshal([]byte(`{"title":"weekly triage"}`), &absent))
	assert.Nil(t, absent.Metadata)

	var cleared api.RecurrenceTemplateUpdateInput
	require.NoError(t, json.Unmarshal([]byte(`{"metadata":null}`), &cleared))
	assert.Nil(t, cleared.Metadata)

	var set api.RecurrenceTemplateUpdateInput
	require.NoError(t, json.Unmarshal([]byte(`{"metadata":{"area":"example-workspace"}}`), &set))
	require.NotNil(t, set.Metadata)
	assert.JSONEq(t, `{"area":"example-workspace"}`, string(*set.Metadata))
}

// TestJSONRawMapDecodesPerKeyRawValues pins that the patch/metadata map form
// still hands each key's value to the caller as undecoded bytes — the daemon
// validates reserved keys through their own type validators and passes
// unknown keys through opaquely.
func TestJSONRawMapDecodesPerKeyRawValues(t *testing.T) {
	var m api.JSONRawMap
	require.NoError(t, json.Unmarshal([]byte(`{"someday":true,"checklist":[{"text":"a"}]}`), &m))

	require.Contains(t, m, "someday")
	assert.Equal(t, "true", string(m["someday"]))
	require.Contains(t, m, "checklist")
	assert.JSONEq(t, `[{"text":"a"}]`, string(m["checklist"]))
}
