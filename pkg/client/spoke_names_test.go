package client

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestFederationSpokeNameEventRoundTrip(t *testing.T) {
	var event generated.EventEnvelope
	require.NoError(t, json.Unmarshal([]byte(`{"origin_instance_uid":"01HZNQ7VFPK1XGD8R5MABCD4EA","origin_spoke_name":"laptop"}`), &event))
	raw, err := json.Marshal(event)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Equal(t, "laptop", wire["origin_spoke_name"])
}
