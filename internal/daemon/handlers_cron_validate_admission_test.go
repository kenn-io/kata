package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Preflight must reject the same missing/null boolean that save rejects,
// while an explicit false remains a savable paused job.
func TestCronValidateJobAdmission(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	for _, enabled := range []string{"missing", "null", "false", "true"} {
		t.Run(enabled, func(t *testing.T) {
			raw, err := json.Marshal(cronHTTPDefinition(t))
			require.NoError(t, err)
			var definition map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(raw, &definition))
			delete(definition, "enabled")
			if enabled != "missing" {
				definition["enabled"] = jsontext.Value(enabled)
			}
			body := map[string]any{"name": "Inspect", "definition": definition, "actor": "operator"}
			path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID)
			status, raw := cronHTTP(t, env, http.MethodPut, path+"/"+cronHTTPUID(t), body)
			savable := enabled == "false" || enabled == "true"
			if savable {
				require.Equal(t, http.StatusOK, status, string(raw))
			} else {
				require.Equal(t, http.StatusBadRequest, status, string(raw))
				require.Contains(t, string(raw), "enabled")
			}
			status, raw = cronHTTP(t, env, http.MethodPost, path+"/validate", body)
			require.Equal(t, http.StatusOK, status, string(raw))
			var out db.CronValidation
			require.NoError(t, json.Unmarshal(raw, &out))
			require.Equal(t, savable, out.Valid, string(raw))
			if !savable {
				require.Len(t, out.Errors, 1)
				require.Equal(t, "document", out.Errors[0].Stage)
				require.Contains(t, out.Errors[0].Message, "enabled")
			}
		})
	}
}

func TestCronValidateCountSchema(t *testing.T) {
	operation := daemon.OpenAPIDocument().Paths["/api/v1/projects/{project_id}/cron/jobs/validate"].Post
	for _, parameter := range operation.Parameters {
		if parameter.Name != "count" {
			continue
		}
		require.Equal(t, "integer", parameter.Schema.Type)
		require.NotNil(t, parameter.Schema.Minimum)
		require.Equal(t, float64(1), *parameter.Schema.Minimum)
		require.NotNil(t, parameter.Schema.Maximum)
		require.Equal(t, float64(100), *parameter.Schema.Maximum)
		return
	}
	t.Fatal("count query parameter missing")
}
