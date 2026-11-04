package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronValidateDecodedInputParity(t *testing.T) {
	f := newCronFixture(t)
	for _, name := range []string{"missing-enabled", "null-enabled", "false-enabled", "true-enabled", "null-after"} {
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
			case "null-after":
				resource, definition, decoded = "workflow", cronWorkflowDefinition(), &generated.CronWorkflowDefinition{}
				definition["steps"].([]any)[0].(map[string]any)["after"] = nil
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
