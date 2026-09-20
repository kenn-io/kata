package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCrossProjectCommandsUseAllFlag(t *testing.T) {
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
