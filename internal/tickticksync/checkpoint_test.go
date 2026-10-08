package tickticksync

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
)

// Contract: a private durable checkpoint and its binding container must be
// JSON objects. Invalid state must fail rather than reset source versions.
func TestCheckpointObjectContract(t *testing.T) {
	for _, raw := range []string{"null", "[]", `{"project_id":"project-1","_provider_checkpoint":null}`, `{"project_id":"project-1","_provider_checkpoint":[]}`} {
		t.Run(raw, func(t *testing.T) {
			_, err := DecodeCheckpoint(jsontext.Value(raw))
			require.Error(t, err)
		})
	}
	for _, raw := range []string{"null", "[]"} {
		t.Run("stage-"+raw, func(t *testing.T) {
			require.NotPanics(t, func() { _, err := WithCheckpoint(jsontext.Value(raw), Checkpoint{}); require.Error(t, err) })
		})
	}
}
