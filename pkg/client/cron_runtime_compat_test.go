package client_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// Additive routes must preserve existing generated client enum assignments.
func TestCronRuntimeKeepsExistingEnumConstants(t *testing.T) {
	check := func(unavailable generated.FederationProjectStatusRootSigningKey, stopping generated.IdleShutdownHealthState, expired, live generated.TokenOutState) {
		if unavailable != "unavailable" || stopping != "stopping" || expired != "expired" || live != "live" {
			t.Fatal("existing wire enum changed")
		}
	}
	check(generated.Unavailable, generated.Stopping, generated.Expired, generated.Live)
}

func TestCronRuntimeChoicesAreAdditive(t *testing.T) {
	choices := map[string][]string{"options.agent": {"review"}}
	beat := generated.BeatCronRuntimeBody{Label: "worker", Adapter: "example", Choices: choices}
	beatJSON, err := json.Marshal(beat)
	require.NoError(t, err)
	assertCronRuntimeJSONField(t, beatJSON, "choices", `{"options.agent":["review"]}`)
	var decodedBeat generated.BeatCronRuntimeBody
	require.NoError(t, json.Unmarshal(beatJSON, &decodedBeat))
	require.Equal(t, choices, decodedBeat.Choices)

	runtime := generated.Runtime{Choices: choices}
	runtimeJSON, err := json.Marshal(runtime)
	require.NoError(t, err)
	assertCronRuntimeJSONField(t, runtimeJSON, "choices", `{"options.agent":["review"]}`)
	var decodedRuntime generated.Runtime
	require.NoError(t, json.Unmarshal(runtimeJSON, &decodedRuntime))
	require.Equal(t, choices, decodedRuntime.Choices)

	metadata := generated.ProjectFederationBody{Features: []string{"cron_runtime_choices_v1"}}
	metadataJSON, err := json.Marshal(metadata)
	require.NoError(t, err)
	assertCronRuntimeJSONField(t, metadataJSON, "features", `["cron_runtime_choices_v1"]`)
	var decodedMetadata generated.ProjectFederationBody
	require.NoError(t, json.Unmarshal(metadataJSON, &decodedMetadata))
	require.Equal(t, []string{"cron_runtime_choices_v1"}, decodedMetadata.Features)
}

func assertCronRuntimeJSONField(t *testing.T, object []byte, name, want string) {
	t.Helper()
	var fields map[string]any
	require.NoError(t, json.Unmarshal(object, &fields))
	value, ok := fields[name]
	require.True(t, ok, "missing %q in %s", name, object)
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	require.JSONEq(t, want, string(raw))
}
