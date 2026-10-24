package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Agents must be able to discover complete workflow creation instructions
// without launching an interactive TUI or guessing a JSON request body.
func TestCronWorkflowCreationDiscoverable(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"tui", "--help"}, {"quickstart"}, {"--agent", "quickstart"}, {"--json", "quickstart"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			resetFlags(t)
			t.Setenv("KATA_HOME", t.TempDir())
			out := string(executeRoot(t, newRootCmd(), args...))
			if args[0] == "--json" {
				var payload struct {
					Quickstart string `json:"quickstart"`
				}
				require.NoError(t, json.Unmarshal([]byte(out), &payload))
				out = payload.Quickstart
			}
			require.Contains(t, out, "kata cron --help")
			require.Contains(t, out, "kata cron workflow create --help")
		})
	}
}
