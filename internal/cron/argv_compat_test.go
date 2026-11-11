package cron_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

// Historical decoding must not turn a kind-only future document into a
// workflow that this release claims it can retain and project.
func TestArgvCompatStoredDecodeRefusesReservedKind(t *testing.T) {
	for _, input := range []string{
		`null`,
		`{"version":2,"steps":[]}`,
		`{"version":1,"steps":[{"kind":"future"}]}`,
		`{"steps":null}`,
		`{"steps":[null]}`,
		`{"steps":[{"kind":null}]}`,
		`{"version":1,"steps":[{"key":"literal","kind":"argv"}]}`,
		`{"version":1,"steps":[{"key":"first","kind":"command","command":"true"},{"key":"literal","kind":"\u0061rgv"}]}`,
		`{"version":1,"steps":[{"key":"literal","kind":"argv","argv":["printf","%s",""]}]}`,
	} {
		_, err := cron.DecodeWorkflow([]byte(input))
		require.Error(t, err)
	}
}

func TestArgvCompatHistoricalDecodeKeepsLegacyRules(t *testing.T) {
	// Current authoring rejects this retry count; accepted history still decodes.
	input := []byte(`{"version":1,"steps":[{"key":"legacy","kind":"command","command":"true","retries":101}]}`)
	workflow, err := cron.DecodeWorkflow(input)
	require.NoError(t, err)
	require.Equal(t, 101, workflow.Steps[0].Retries)
	_, err = cron.ParseWorkflow(input)
	require.Error(t, err)
}
