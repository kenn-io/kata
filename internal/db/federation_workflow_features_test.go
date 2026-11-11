package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Workflow event membership follows the declared registry, so every declared
// workflow type is fenced and no other declared type is.
func TestWorkflowDefinitionEventFollowsRegistry(t *testing.T) {
	workflows := 0
	for _, declared := range federationEventTypes {
		isWorkflow := strings.HasPrefix(declared.name, "cron.workflow.")
		assert.Equal(t, isWorkflow, workflowDefinitionEvent(declared.name), declared.name)
		if !isWorkflow {
			continue
		}
		workflows++
		argv, err := payloadHasArgvWorkflow(declared.name, `{"definition":{"version":1,"steps":[{"kind":"argv"}]}}`)
		require.NoError(t, err, declared.name)
		assert.True(t, argv, declared.name)
	}
	require.NotZero(t, workflows)
	assert.False(t, workflowDefinitionEvent("cron.workflow.undeclared"))
}
