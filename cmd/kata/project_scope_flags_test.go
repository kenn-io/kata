package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestCrossProjectCommandsUseAllFlag(t *testing.T) { //nolint:paralleltest // newRootCmd resets package var flags
	for _, command := range [][]string{{"inbox"}, {"events"}, {"digest"}, {"mcp", "serve"}} {
		t.Run(strings.Join(command, "_"), func(t *testing.T) {
			args := append(append([]string{}, command...), "--all", "--help")
			out, err := runCmdOutput(t, nil, args...)
			require.NoError(t, err)
			assert.Contains(t, out, "--all ")
			assert.NotContains(t, out, "--all-projects")

			args = append(append([]string{}, command...), "--all-projects", "--help")
			_, err = runCmdOutput(t, nil, args...)
			require.ErrorContains(t, err, "unknown flag: --all-projects")
		})
	}
}

func TestCrossProjectCommandsRejectExplicitScope(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	for _, command := range [][]string{
		{"inbox", "--for", "reviewer"}, {"list"}, {"ready"}, {"next"},
		{"events"}, {"digest", "--since", "1h"}, {"mcp", "serve"},
	} {
		for _, scope := range []string{"--project", "--workspace"} {
			t.Run(command[0]+scope, func(t *testing.T) {
				env := testenv.New(t)
				value := "spoke-project"
				if scope == "--workspace" {
					value = t.TempDir()
				}
				args := append(append([]string{}, command...), "--all", scope, value)
				_, err := runCmdOutput(t, env, args...)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "--all")
				assert.Contains(t, err.Error(), scope)
			})
		}
	}
}
