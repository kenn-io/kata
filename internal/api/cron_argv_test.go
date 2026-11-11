package api

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

func TestWorkflowArgvConversion(t *testing.T) {
	argv := []string{""}
	original := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "literal", Kind: "argv", Argv: argv}}}
	wire := CronWorkflowDefinitionFrom(original)
	require.Equal(t, argv, wire.Steps[0].Argv)
	require.Equal(t, original, wire.Native())
	for _, value := range []any{cron.WorkflowStep{}, CronWorkflowStep{}} {
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		require.NotContains(t, string(raw), `"argv"`)
	}
	for _, value := range []any{cron.WorkflowStep{Argv: []string{}}, CronWorkflowStep{Argv: []string{}}} {
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"argv":[]`)
	}
}
