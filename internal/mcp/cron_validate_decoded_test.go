package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronValidateDecodedInputParity(t *testing.T) {
	f := newCronFixture(t)
	for _, name := range []string{"missing-enabled", "null-enabled", "false-enabled", "true-enabled", "null-after", "null-options", "null-workflow-options", "null-step-options"} {
		t.Run(name, func(t *testing.T) {
			resource := "job"
			definition := cronDefinition()
			var decoded any = &generated.CronJobDefinition{}
			switch name {
			case "missing-enabled":
				delete(definition, "enabled")
			case "null-enabled":
				definition["enabled"] = nil
			case "true-enabled":
				definition["enabled"] = true
			case "null-options":
				definition["options"] = nil
			case "null-workflow-options", "null-step-options":
				resource, definition, decoded = "workflow", cronWorkflowDefinition(), &generated.CronWorkflowDefinition{}
				if name == "null-workflow-options" {
					definition["options"] = nil
				} else {
					definition["steps"].([]any)[0].(map[string]any)["options"] = nil
				}
			case "null-after":
				resource, definition, decoded = "workflow", cronWorkflowDefinition(), &generated.CronWorkflowDefinition{}
				definition["steps"].([]any)[0].(map[string]any)["after"] = nil
			}
			if name == "missing-enabled" || name == "null-enabled" || name == "false-enabled" || name == "true-enabled" {
				created := cronCall(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": name, "definition": definition})
				savable := name == "false-enabled" || name == "true-enabled"
				require.Equal(t, !savable, created.IsError)
				if !savable {
					require.Contains(t, string(mustJSON(t, created.Content)), "enabled")
				}
			}
			require.NoError(t, json.Unmarshal(mustJSON(t, definition), decoded))
			// Mutation schemas remain strict. Compare the typed save input, as
			// SDK clients send it, with validation of the original definition.
			saved := cronRecord(t, f.session, "cron_"+resource+"_create", map[string]any{"uid": cronNewUID(t, f.session), "name": name, "definition": decoded}, resource)
			result := cronOK(t, f.session, "cron_"+resource+"_validate", map[string]any{"name": name, "definition": definition})
			require.Equal(t, true, result["valid"])
			require.JSONEq(t, string(mustJSON(t, saved["definition"])), string(mustJSON(t, result["definition"])))
			if resource == "job" {
				require.Equal(t, name == "true-enabled", result["definition"].(map[string]any)["enabled"])
			}
			if resource == "job" && name != "true-enabled" {
				require.Equal(t, "paused", result["warnings"].([]any)[0].(map[string]any)["code"])
			}
		})
	}
}
