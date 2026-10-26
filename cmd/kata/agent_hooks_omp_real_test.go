package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOmpRealLoader(t *testing.T) {
	if os.Getenv("KATA_TEST_OMP") != "1" {
		t.Skip("set KATA_TEST_OMP=1 for isolated installed OMP loader checks")
	}
	binary, err := exec.LookPath("omp")
	if err != nil {
		t.Skip("omp unavailable")
	}
	plugin := os.Getenv("KATA_TEST_OMP_PLUGIN")
	require.NotEmpty(t, plugin, "set KATA_TEST_OMP_PLUGIN to the released oh-my-pi plugin directory")
	generated, err := generateOmpAgentHooks(ompAgentHookMetadata{Format: "kata-omp", Version: 1, Scope: "user", Executable: "kata", Plan: true})
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "kata.mjs")
	require.NoError(t, os.WriteFile(file, generated, 0600))
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	command := exec.Command(node, "testdata/agent-hooks/omp-real-loader.mjs", file, binary, plugin) //nolint:gosec // G204: fixed fixture invokes the explicitly selected OMP and plugin test inputs without a shell.
	t.Log(string(runPlanNode(t, command)))
}
