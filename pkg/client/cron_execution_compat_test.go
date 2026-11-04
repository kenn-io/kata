package client_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// Additive cron capabilities must preserve existing public client enum symbols.
func TestCronExecutionPreservesClientEnums(t *testing.T) {
	values := struct {
		visibility generated.SetProjectAccessRequestBodyVisibility
		repin      generated.FederationRootKeyRepinResultMethod
	}{visibility: generated.All, repin: generated.None}
	require.Equal(t, "all", string(values.visibility))
	require.Equal(t, "none", string(values.repin))
}

// Older daemon responses must remain identifiable by their absent policy.
func TestCronExecutionOlderDaemonResponse(t *testing.T) {
	var response generated.GetCronCapabilitiesResponse
	require.NoError(t, json.Unmarshal([]byte(`{"project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","event_features":["cron_v1"]}`), &response))
	require.NoError(t, response.Validate())
	raw, err := json.Marshal(response)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	require.NotContains(t, body, "execution")
}
