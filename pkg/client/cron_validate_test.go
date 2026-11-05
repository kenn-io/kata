package client_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// Preflight carries invalid shapes and opaque numbers without client coercion.
func TestCronValidationSDKRawDefinition(t *testing.T) {
	for _, definition := range []string{`null`, `[]`, `1`, `{"options":{"n":12345678901234567890123,"d":0.12345678901234567890123}}`} {
		var request generated.CronValidationBody
		require.NoError(t, json.Unmarshal([]byte(`{"name":"Inspect","definition":`+definition+`}`), &request))
		raw, err := json.Marshal(request)
		require.NoError(t, err)
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &fields))
		require.Equal(t, definition, string(fields["definition"]))
	}
	var missing generated.CronValidationBody
	require.NoError(t, json.Unmarshal([]byte(`{}`), &missing))
	rawMissing, err := json.Marshal(missing)
	require.NoError(t, err)
	var missingFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rawMissing, &missingFields))
	require.NotContains(t, missingFields, "definition")
	var result generated.CronValidationResult
	require.NoError(t, json.Unmarshal([]byte(`{"valid":true,"name":"Inspect","definition":{"n":12345678901234567890123,"d":0.12345678901234567890123},"errors":[],"warnings":[]}`), &result))
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	require.Contains(t, string(raw), "12345678901234567890123")
	require.Contains(t, string(raw), "0.12345678901234567890123")
}

// Required-field validation accepts present empty names and zero instants,
// rejects absent/null fields, and validates nested diagnostics and fires.
func TestCronValidationSDKPresence(t *testing.T) {
	for _, tc := range []struct {
		name            string
		request, reject bool
		raw             string
	}{
		{"collision-zero-until", false, false, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"adapter_trigger","zone":"UTC","from":"0000-12-25T00:00:00Z","next":[],"collisions":{"until":"0001-01-01T00:00:00Z","items":[],"skipped":0,"truncated":false}}}`},
		{"collision-zero-at", false, false, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"adapter_trigger","zone":"UTC","from":"0001-01-01T00:00:00Z","next":[],"collisions":{"until":"0001-01-08T00:00:00Z","items":[{"at":"0001-01-01T00:00:00Z","local":"0001-01-01T00:00:00Z","occurrences":1,"peer_count":1,"peers":[{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","name":"Peer"}]}],"skipped":0,"truncated":false}}}`},
		{"collision-missing-until", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"adapter_trigger","zone":"UTC","from":"0001-01-01T00:00:00Z","next":[],"collisions":{"items":[{"at":"0001-01-01T00:00:00Z","local":"0001-01-01T00:00:00Z","occurrences":1,"peer_count":1,"peers":[{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","name":"Peer"}]}],"skipped":0,"truncated":false}}}`},
		{"collision-null-until", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"adapter_trigger","zone":"UTC","from":"0001-01-01T00:00:00Z","next":[],"collisions":{"until":null,"items":[{"at":"0001-01-01T00:00:00Z","local":"0001-01-01T00:00:00Z","occurrences":1,"peer_count":1,"peers":[{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","name":"Peer"}]}],"skipped":0,"truncated":false}}}`},
		{"collision-missing-at", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"adapter_trigger","zone":"UTC","from":"0001-01-01T00:00:00Z","next":[],"collisions":{"until":"0001-01-08T00:00:00Z","items":[{"local":"0001-01-01T00:00:00Z","occurrences":1,"peer_count":1,"peers":[{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","name":"Peer"}]}],"skipped":0,"truncated":false}}}`},
		{"collision-null-at", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"adapter_trigger","zone":"UTC","from":"0001-01-01T00:00:00Z","next":[],"collisions":{"until":"0001-01-08T00:00:00Z","items":[{"at":null,"local":"0001-01-01T00:00:00Z","occurrences":1,"peer_count":1,"peers":[{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","name":"Peer"}]}],"skipped":0,"truncated":false}}}`},
		{"collision-invalid-peer", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"adapter_trigger","zone":"UTC","from":"0001-01-01T00:00:00Z","next":[],"collisions":{"until":"0001-01-08T00:00:00Z","items":[{"at":"0001-01-01T00:00:00Z","local":"0001-01-01T00:00:00Z","occurrences":1,"peer_count":1,"peers":[{}]}],"skipped":0,"truncated":false}}}`},
		{"empty-name", false, false, `{"valid":false,"name":"","definition":null,"errors":[{"stage":"request","code":"validation","message":"name required"}],"warnings":[]}`},
		{"nested-0", false, true, `{"name":"Inspect","definition":null,"errors":[{}],"warnings":[]}`},
		{"nested-1", false, true, `{"name":"Inspect","definition":null,"errors":[],"warnings":[{}]}`},
		{"nested-2", false, true, `{"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"basis":"adapter_trigger","delivery":"adapter","zone":"UTC","from":"2026-11-03T00:00:00Z","next":[{"at":"2026-11-04T00:00:00Z"}]}}`},
		{"request-empty-name", true, false, `{"name":"","definition":null}`},
		{"zero-from", false, false, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"basis":"fresh_anchor","delivery":"daemon","zone":"UTC","from":"0001-01-01T00:00:00Z","next":[{"at":"0001-01-01T00:01:00Z","local":"0001-01-01T00:01:00Z"}]}}`},
		{"zero-fire", false, false, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"basis":"fresh_anchor","delivery":"daemon","zone":"UTC","from":"0000-12-31T23:59:59Z","next":[{"at":"0001-01-01T00:00:00Z","local":"0001-01-01T00:00:00Z"}]}}`},
		{"missing-instant-0", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"basis":"adapter_trigger","delivery":"adapter","zone":"UTC","next":[]}}`},
		{"missing-instant-1", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"basis":"adapter_trigger","delivery":"adapter","zone":"UTC","from":null,"next":[]}}`},
		{"missing-instant-2", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"basis":"adapter_trigger","delivery":"adapter","zone":"UTC","from":"2026-11-04T00:00:00Z","next":[{"local":"2026-11-04T00:01:00Z"}]}}`},
		{"missing-instant-3", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"basis":"adapter_trigger","delivery":"adapter","zone":"UTC","from":"2026-11-04T00:00:00Z","next":[{"at":null,"local":"2026-11-04T00:01:00Z"}]}}`},
		{"missing-name", false, true, `{"valid":false,"definition":null,"errors":[],"warnings":[]}`},
		{"null-name", false, true, `{"name":null,"valid":false,"definition":null,"errors":[],"warnings":[]}`},
		{"missing-errors", false, true, `{"valid":true,"name":"Inspect","definition":{},"warnings":[],"schedule":{"delivery":"adapter","basis":"none","zone":"UTC","from":"2026-11-03T00:00:00Z","next":[]}}`},
		{"null-errors", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":null,"warnings":[],"schedule":{"delivery":"adapter","basis":"none","zone":"UTC","from":"2026-11-03T00:00:00Z","next":[]}}`},
		{"missing-warnings", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"schedule":{"delivery":"adapter","basis":"none","zone":"UTC","from":"2026-11-03T00:00:00Z","next":[]}}`},
		{"null-warnings", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":null,"schedule":{"delivery":"adapter","basis":"none","zone":"UTC","from":"2026-11-03T00:00:00Z","next":[]}}`},
		{"missing-delivery", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"basis":"none","zone":"UTC","from":"2026-11-03T00:00:00Z","next":[]}}`},
		{"missing-basis", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"delivery":"adapter","zone":"UTC","from":"2026-11-03T00:00:00Z","next":[]}}`},
		{"missing-zone", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"none","from":"2026-11-03T00:00:00Z","next":[]}}`},
		{"missing-next", false, true, `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"none","zone":"UTC","from":"2026-11-03T00:00:00Z"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.request {
				var body generated.CronValidationBody
				require.NoError(t, json.Unmarshal([]byte(tc.raw), &body))
				err = body.Validate()
			} else {
				var result generated.CronValidationResult
				require.NoError(t, json.Unmarshal([]byte(tc.raw), &result))
				err = result.Validate()
			}
			if tc.reject {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
