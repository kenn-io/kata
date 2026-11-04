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

// An empty required name is a legitimate request-stage validation outcome.
func TestCronValidationSDKEmptyName(t *testing.T) {
	var result generated.CronValidationResult
	require.NoError(t, json.Unmarshal([]byte(`{"valid":false,"name":"","definition":null,"errors":[{"stage":"request","code":"validation","message":"name required"}],"warnings":[]}`), &result))
	require.NoError(t, result.Validate())
}

// SDK result validation must reject malformed nested diagnostics and fires.
func TestCronValidationSDKNestedResponses(t *testing.T) {
	for _, raw := range []string{
		`{"name":"Inspect","definition":null,"errors":[{}],"warnings":[]}`,
		`{"name":"Inspect","definition":null,"errors":[],"warnings":[{}]}`,
		`{"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"basis":"adapter_trigger","delivery":"adapter","zone":"UTC","from":"2026-11-03T00:00:00Z","next":[{"at":"2026-11-04T00:00:00Z"}]}}`,
	} {
		var result generated.CronValidationResult
		require.NoError(t, json.Unmarshal([]byte(raw), &result))
		require.Error(t, result.Validate(), raw)
	}
}

func TestCronValidationSDKRequestStageEnvelope(t *testing.T) {
	var body generated.CronValidationBody
	require.NoError(t, json.Unmarshal([]byte(`{"name":"","definition":null}`), &body))
	require.NoError(t, body.Validate())
}

func TestCronValidationSDKZeroInstants(t *testing.T) {
	for _, tc := range []struct{ name, from, at string }{
		{"from", "0001-01-01T00:00:00Z", "0001-01-01T00:01:00Z"},
		{"fire", "0000-12-31T23:59:59Z", "0001-01-01T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":{"basis":"fresh_anchor","delivery":"daemon","zone":"UTC","from":"` + tc.from + `","next":[{"at":"` + tc.at + `","local":"` + tc.at + `"}]}}`
			var result generated.CronValidationResult
			require.NoError(t, json.Unmarshal([]byte(raw), &result))
			require.NoError(t, result.Validate())
		})
	}
}

func TestCronValidationSDKMissingInstants(t *testing.T) {
	for _, schedule := range []string{
		`{"basis":"adapter_trigger","delivery":"adapter","zone":"UTC","next":[]}`,
		`{"basis":"adapter_trigger","delivery":"adapter","zone":"UTC","from":null,"next":[]}`,
		`{"basis":"adapter_trigger","delivery":"adapter","zone":"UTC","from":"2026-11-04T00:00:00Z","next":[{"local":"2026-11-04T00:01:00Z"}]}`,
		`{"basis":"adapter_trigger","delivery":"adapter","zone":"UTC","from":"2026-11-04T00:00:00Z","next":[{"at":null,"local":"2026-11-04T00:01:00Z"}]}`,
	} {
		var result generated.CronValidationResult
		require.NoError(t, json.Unmarshal([]byte(`{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[],"schedule":`+schedule+`}`), &result))
		require.Error(t, result.Validate(), schedule)
	}
}

// Required response names distinguish an empty value from an absent value.
func TestCronValidationSDKMissingNames(t *testing.T) {
	for _, name := range []string{"", `"name":null,`} {
		var result generated.CronValidationResult
		require.NoError(t, json.Unmarshal([]byte(`{`+name+`"valid":false,"definition":null,"errors":[],"warnings":[]}`), &result))
		require.Error(t, result.Validate())
	}
}
