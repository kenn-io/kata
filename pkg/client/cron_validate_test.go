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
