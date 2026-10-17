package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHistoricalAgentReferencesQuoteProjectNames(t *testing.T) {
	// These commands exercise the shared mutation, show, delete, and purge
	// rendering boundaries. Other mutations use the same shared printer.
	for _, operation := range []string{"show", "edit", "delete", "purge"} {
		t.Run(operation, func(t *testing.T) {
			env, dir, target, moved, oldRef := historicalOutputFixtureWithTarget(t, `target project "review"`)
			qualified := target.Name + "#" + moved.ShortID
			args := []string{operation, oldRef}
			switch operation {
			case "edit":
				args = append(args, "--title", "Continued work")
			case "delete", "purge":
				args = append(args, "--force", "--confirm", strings.ToUpper(operation)+" "+qualified)
			}
			output := runCLI(t, env, dir, append(args, "--agent")...)
			lines := strings.Split(output, "\n")
			var displayed string
			for _, prefix := range []string{"OK " + operation + " ", "Issue: "} {
				var line string
				for _, candidate := range lines {
					if value, ok := strings.CutPrefix(candidate, prefix); ok {
						line = value
						break
					}
				}
				require.NotEmpty(t, line, output)
				encoded, err := jsontext.NewDecoder(strings.NewReader(line)).ReadValue()
				require.NoError(t, err, "agent references with spaces and quotes must be a quoted value: %s", output)
				require.NoError(t, json.Unmarshal(encoded, &displayed))
				require.Equal(t, qualified, displayed)
			}
			if operation == "purge" {
				return // The receipt identifies the removed issue in its former project.
			}
			if operation == "delete" {
				runCLI(t, env, dir, "restore", displayed)
			}
			var response struct {
				Issue struct {
					UID string `json:"uid"`
				} `json:"issue"`
			}
			require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "show", displayed, "--json")), &response))
			require.Equal(t, moved.UID, response.Issue.UID, "decoded agent reference must roundtrip to the moved issue")
		})
	}
}
