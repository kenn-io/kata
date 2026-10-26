package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOmpDelegateRealDaemon(t *testing.T) {
	cmd := planNode(t, "omp-delegate-real.mjs")
	if filter := os.Getenv("KATA_TEST_OMP_FILTER"); filter != "" {
		cmd.Args = append(cmd.Args, filter)
	}
	binary := planTestBinary(t)
	generated, err := generateOmpAgentHooks(ompAgentHookMetadata{Format: "kata-omp", Version: 1, Scope: "user", Executable: binary, Plan: true})
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "kata.mjs")
	require.NoError(t, os.WriteFile(file, generated, 0600))
	cmd.Env = append(cmd.Env, "KATA_TEST_EXECUTABLE="+binary, "KATA_TEST_OMP_ADAPTER="+file)
	t.Log(string(runPlanNode(t, cmd)))
}
