package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestAgentHooksStatusBareExecutableMissingFromPath(t *testing.T) {
	isolateAgentHookHomes(t)
	workspace := t.TempDir()
	t.Chdir(workspace)
	t.Setenv("PATH", t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "example-kata"), []byte("fixture"), 0o600))
	config := filepath.Join(t.TempDir(), "hooks.json")
	installHookFixture(t, agenthook.AgentCodex, config, "example-kata", agentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	// Inspect one config directly so the fixture does not need git on its PATH.
	status, err := readAgentHookStatus(agenthook.AgentCodex, config, workspace)
	require.NoError(t, err)
	require.Len(t, status.Entries, 1)
	require.Equal(t, "example-kata", status.Entries[0].Executable)
	require.False(t, status.Entries[0].ExecutableExists)
}
