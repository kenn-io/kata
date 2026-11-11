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
		`{"version":1,"steps":[{"key":"literal","kind":"argv"}]}`,
		`{"version":1,"steps":[{"key":"first","kind":"command","command":"true"},{"key":"literal","kind":"\u0061rgv"}]}`,
	} {
		_, err := cron.DecodeWorkflow([]byte(input))
		require.Error(t, err)
		_, err = cron.ParseWorkflow([]byte(input))
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

func TestArgvMemberRemainsRefused(t *testing.T) {
	for _, member := range []string{`"argv":null`, `"argv":[]`, `"argv":["echo", ""]`} {
		for _, step := range []string{`"kind":"command","command":"true"`, `"kind":"prompt","prompt":"hello"`} {
			raw := []byte(`{"version":1,"steps":[{"key":"step",` + step + `,` + member + `}]}`)
			_, err := cron.DecodeHistoricalWorkflow(raw)
			require.Error(t, err, string(raw))
			_, err = cron.DecodeWorkflow(raw)
			require.Error(t, err, string(raw))
			_, err = cron.ParseWorkflow(raw)
			require.Error(t, err, string(raw))
		}
	}
	for _, argv := range [][]string{{}, {"echo", ""}} {
		workflow := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "step", Kind: "command", Command: "true", Argv: argv}}}
		require.Error(t, workflow.Validate())
		require.Error(t, cron.CheckWorkflowSupported(workflow))
	}
}

func TestArgvWorkflowContract(t *testing.T) {
	valid := []byte(`{"version":1,"steps":[{"key":"literal","kind":"argv","argv":["printf","", "line\nnext"]}]}`)
	for _, decode := range []func([]byte) (cron.WorkflowDefinition, error){cron.ParseWorkflow, cron.DecodeWorkflow} {
		workflow, err := decode(valid)
		require.NoError(t, err)
		require.Equal(t, []string{"printf", "", "line\nnext"}, workflow.Steps[0].Argv)
		require.NoError(t, workflow.Validate())
		require.NoError(t, cron.CheckWorkflowSupported(workflow))
	}
	for _, step := range []string{
		`"kind":"argv"`, `"kind":"argv","argv":null`, `"kind":"argv","argv":[]`,
		`"kind":"argv","argv":[""]`, `"kind":"argv","argv":["x",null]`,
		`"kind":"argv","argv":["x",1]`, `"kind":"argv","argv":["x","\u0000"]`,
		`"kind":"argv","argv":["x"],"command":""`, `"kind":"argv","argv":["x"],"prompt":""`,
	} {
		raw := []byte(`{"version":1,"steps":[{"key":"step",` + step + `}]}`)
		for _, decode := range []func([]byte) (cron.WorkflowDefinition, error){cron.ParseWorkflow, cron.DecodeWorkflow} {
			_, err := decode(raw)
			require.Error(t, err, string(raw))
		}
	}
}
