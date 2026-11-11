package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func argvRequest(t *testing.T, env *testenv.Env, method, path, body, features string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, env.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	if features != "" {
		req.Header.Set(db.EventFeaturesHeader, features)
	}
	resp, err := env.HTTP.Do(req) //nolint:gosec // The URL is the isolated test server.
	require.NoError(t, err)
	return resp.StatusCode, string(readClose(t, resp))
}

func TestCronWorkflowArgvHTTPCompatibility(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/workflows", project.ID)
	body := `{"actor":"worker","name":"Literal","definition":{"version":1,"steps":[{"key":"run","kind":"argv","argv":["printf",""]}]}}`
	status, raw := argvRequest(t, env, http.MethodPost, path, body, "")
	require.Equalf(t, http.StatusCreated, status, "%s", raw)
	var created struct {
		Workflow struct {
			UID                string `json:"uid"`
			DefinitionEventUID string `json:"definition_event_uid"`
		} `json:"workflow"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &created))
	require.NotEmpty(t, created.Workflow.UID)
	for _, features := range []string{"", "cron_workflow_argv_v1_extra"} {
		for _, readPath := range []string{path, path + "/" + created.Workflow.UID} {
			status, raw = argvRequest(t, env, http.MethodGet, readPath, "", features)
			require.Equalf(t, http.StatusConflict, status, "%s", raw)
			require.Contains(t, raw, "unsupported_event_features")
			require.NotContains(t, raw, "printf")
		}
	}
	status, raw = argvRequest(t, env, http.MethodGet, path, "", "cron_v1, "+db.CronWorkflowArgvEventFeature)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	require.Contains(t, raw, `"argv":["printf",""]`)

	runPath := fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t))
	run := fmt.Sprintf(`{"actor":"worker","workflow_uid":%q,"workflow_definition_event_uid":%q,"status":"running","summary":{"version":1},"expected_revision":0}`, created.Workflow.UID, created.Workflow.DefinitionEventUID)
	status, raw = argvRequest(t, env, http.MethodPut, runPath, run, "")
	require.Equalf(t, http.StatusConflict, status, "%s", raw)
	require.Contains(t, raw, "unsupported_event_features")
	status, raw = argvRequest(t, env, http.MethodPut, runPath, run, db.CronWorkflowArgvEventFeature)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
}

func TestCronWorkflowArgvHTTPRejectsRawShapes(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/workflows", project.ID)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	// Typed decoding erases these empty members; the HTTP handler must parse RawBody.
	for _, step := range []string{
		`{"key":"run","kind":"argv","argv":["printf"],"command":""}`,
		`{"key":"run","kind":"argv","argv":["printf"],"prompt":""}`,
	} {
		body := `{"actor":"worker","name":"Literal","definition":{"version":1,"steps":[` + step + `]}}`
		status, raw := argvRequest(t, env, http.MethodPost, path, body, db.CronWorkflowArgvEventFeature)
		require.Equalf(t, http.StatusBadRequest, status, "%s: %s", step, raw)
		var response api.ErrorEnvelope
		require.NoError(t, json.Unmarshal([]byte(raw), &response))
		require.Equal(t, "validation", response.Error.Code)
		after, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
}
