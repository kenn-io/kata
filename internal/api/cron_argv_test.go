package api

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

func TestWorkflowArgvConversion(t *testing.T) {
	argv := []string{"printf", "", "two words", "two words", "'$(x);", "雪", "line\nbreak"}
	original := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "literal", Kind: "argv", Argv: argv}}}
	wire := CronWorkflowDefinitionFrom(original)
	require.Equal(t, argv, wire.Steps[0].Argv)
	require.Equal(t, original, wire.Native())
}
