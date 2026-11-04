package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestCronValidateWorkflowAdmission(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/workflows", project.ID)
	for _, after := range []string{"missing", "null", "[]", `["prepare"]`} {
		t.Run(after, func(t *testing.T) {
			step := map[string]any{"key": "review", "kind": "prompt", "prompt": "Review"}
			if after != "missing" {
				step["after"] = jsontext.Value(after)
			}
			body := map[string]any{"name": "Inspect", "actor": "operator", "definition": map[string]any{
				"version": 1, "steps": []any{map[string]any{"key": "prepare", "kind": "prompt", "prompt": "Prepare"}, step},
			}}
			savable := after != "null"
			status, raw := cronHTTP(t, env, http.MethodPut, path+"/"+cronHTTPUID(t), body)
			wantStatus := http.StatusOK
			if !savable {
				wantStatus = http.StatusBadRequest
			}
			require.Equal(t, wantStatus, status, string(raw))
			status, raw = cronHTTP(t, env, http.MethodPost, path+"/validate", body)
			require.Equal(t, http.StatusOK, status, string(raw))
			var out db.CronValidation
			require.NoError(t, json.Unmarshal(raw, &out))
			require.Equal(t, savable, out.Valid, string(raw))
			if !savable {
				require.Len(t, out.Errors, 1)
				require.Equal(t, "document", out.Errors[0].Stage)
				require.Contains(t, out.Errors[0].Message, "after")
			}
		})
	}
}
