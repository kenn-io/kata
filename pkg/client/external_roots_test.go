package client_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestGeneratedExternalFieldConflictCandidatesRoundTrip(t *testing.T) {
	input := []byte(`{
		"kata_field":"scheduled_on",
		"kata_candidate":{"kind":"date","value":"2026-08-21"},
		"external_candidate":{"kind":"date","value":"2026-08-22"}
	}`)
	var conflict generated.ExternalFieldConflictOut
	require.NoError(t, json.Unmarshal(input, &conflict))
	require.NotNil(t, conflict.KataCandidate)
	require.NotNil(t, conflict.ExternalCandidate)
	assert.Equal(t, "date", conflict.KataCandidate.Kind)
	require.NotNil(t, conflict.KataCandidate.Value)
	assert.Equal(t, "2026-08-21", *conflict.KataCandidate.Value)
	assert.Equal(t, "date", conflict.ExternalCandidate.Kind)
	require.NotNil(t, conflict.ExternalCandidate.Value)
	assert.Equal(t, "2026-08-22", *conflict.ExternalCandidate.Value)

	encoded, err := json.Marshal(conflict)
	require.NoError(t, err)
	assert.JSONEq(t, string(input), string(encoded))
}
