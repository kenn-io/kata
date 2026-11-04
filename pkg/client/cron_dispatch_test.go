package client_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
	"testing"
)

func TestCronDispatchTypedResponsePreservesNullObservation(t *testing.T) {
	raw := []byte(`{"job_uid":"01K5ZN7SP700000000000000001","evaluated_at":"2026-11-04T00:00:00Z","gate":{"reason":"idle"},"last":null}`)
	var value generated.CronDispatchExplanation
	require.NoError(t, json.Unmarshal(raw, &value))
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(encoded, &fields))
	require.Equal(t, "null", string(fields["last"]))
}
