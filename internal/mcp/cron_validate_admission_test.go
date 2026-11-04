package mcpserver

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCronValidateJobAdmission(t *testing.T) {
	f := newCronFixture(t)
	for _, enabled := range []string{"missing", "null", "false", "true"} {
		t.Run(enabled, func(t *testing.T) {
			definition := cronDefinition()
			delete(definition, "enabled")
			switch enabled {
			case "null":
				definition["enabled"] = nil
			case "false":
				definition["enabled"] = false
			case "true":
				definition["enabled"] = true
			}
			args := map[string]any{"uid": cronNewUID(t, f.session), "name": "Inspect", "definition": definition}
			created := cronCall(t, f.session, "cron_job_create", args)
			savable := enabled == "false" || enabled == "true"
			require.Equal(t, !savable, created.IsError, string(mustJSON(t, created)))
			if savable {
				delete(args, "uid")
			} else {
				require.Contains(t, string(mustJSON(t, created)), "enabled")
			}
			result := cronOK(t, f.session, "cron_job_validate", args)
			require.Equal(t, savable, result["valid"], result)
			if !savable {
				failure := result["errors"].([]any)[0].(map[string]any)
				require.Equal(t, "document", failure["stage"])
				require.Contains(t, failure["message"], "enabled")
			}
		})
	}
}
