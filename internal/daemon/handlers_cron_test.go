package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func cronHTTP(t *testing.T, env *testenv.Env, method, path string, body any) (int, []byte) {
	t.Helper()
	raw := ""
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		raw = string(encoded)
	}
	response := doReqEnv(t, env, method, env.URL+path, raw, "")
	return response.StatusCode, readClose(t, response)
}
func cronHTTPUID(t *testing.T) string {
	t.Helper()
	value, err := uid.New()
	require.NoError(t, err)
	return value
}
func cronHTTPDefinition(_ *testing.T) cron.JobDefinition {
	return cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 60}, Action: cron.Action{Kind: "execute", Prompt: "Inspect"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Inspection result"}, Overlap: "forbid", Catchup: "all", Options: []byte(`{"model":"example"}`)}
}

func TestCronDefinitionAPI(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "cron-example")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	definition := cronHTTPDefinition(t)
	body := map[string]any{"actor": "worker", "name": "Inspect", "definition": definition}
	status, raw := cronHTTP(t, env, http.MethodPost, path, body)
	require.Equalf(t, http.StatusCreated, status, "%s", raw)
	var created struct {
		Job    db.CronJob `json:"job"`
		Events []db.Event `json:"events"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	require.NotEmpty(t, created.Job.UID)
	require.Len(t, created.Events, 1)
	require.JSONEq(t, string(definition.Options), string(created.Job.Definition.Options))
	require.Equal(t, []int64{created.Events[0].ID}, drainBroadcastIDs(t, sub.Ch, 50*time.Millisecond))
	path += "/" + created.Job.UID
	status, raw = cronHTTP(t, env, http.MethodGet, path, nil)
	require.Equalf(t, 200, status, "%s", raw)
	definition.Action.Prompt = "Updated content"
	body["definition"] = definition
	body["expected_event_uid"] = created.Job.DefinitionEventUID
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "%s", raw)
	var updated struct {
		Job    db.CronJob `json:"job"`
		Events []db.Event `json:"events"`
	}
	require.NoError(t, json.Unmarshal(raw, &updated))
	require.Len(t, updated.Events, 1)
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 409, status, "%s", raw)
	require.Contains(t, string(raw), updated.Job.DefinitionEventUID, "conflict includes current definition")
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	other := seedProject(t, env, "other-cron")
	status, raw = cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s", other.ID, created.Job.UID), nil)
	require.Equalf(t, 404, status, "%s", raw)
	action := map[string]any{"actor": "worker", "expected_event_uid": updated.Job.DefinitionEventUID}
	status, raw = cronHTTP(t, env, http.MethodDelete, path, action)
	require.Equalf(t, 200, status, "%s", raw)
	require.NoError(t, json.Unmarshal(raw, &updated))
	require.NotNil(t, updated.Job.DeletedAt)
	action["expected_event_uid"] = updated.Job.DefinitionEventUID
	status, raw = cronHTTP(t, env, http.MethodPost, path+"/restore", action)
	require.Equalf(t, 200, status, "%s", raw)
	require.NotContains(t, string(raw), `"deleted_at"`)
	updated.Job = db.CronJob{} // Decode each response independently; absent fields do not clear reused structs.
	require.NoError(t, json.Unmarshal(raw, &updated))
	require.Nil(t, updated.Job.DeletedAt)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Greater(t, after, before)
	malformed := map[string]any{"actor": "worker", "name": "Invalid", "definition": map[string]any{"version": 99}}
	status, _ = cronHTTP(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID), malformed)
	require.Contains(t, []int{400, 422}, status)
}

func TestCronWorkflowAPI(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "workflow-example")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/workflows", project.ID)
	definition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}
	body := map[string]any{"actor": "worker", "name": "Inspect workflow", "definition": definition}
	status, raw := cronHTTP(t, env, http.MethodPost, path, body)
	require.Equalf(t, 201, status, "%s", raw)
	var response struct {
		Workflow db.CronWorkflow `json:"workflow"`
		Events   []db.Event      `json:"events"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Len(t, response.Events, 1)
	status, raw = cronHTTP(t, env, http.MethodGet, path, nil)
	require.Equalf(t, 200, status, "%s", raw)
	require.Contains(t, string(raw), response.Workflow.UID)
	path += "/" + response.Workflow.UID
	body["expected_event_uid"] = response.Workflow.DefinitionEventUID
	body["name"] = "Edited workflow"
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "%s", raw)
	require.NoError(t, json.Unmarshal(raw, &response))
	action := map[string]any{"actor": "worker", "expected_event_uid": response.Workflow.DefinitionEventUID}
	status, raw = cronHTTP(t, env, http.MethodDelete, path, action)
	require.Equalf(t, 200, status, "%s", raw)
	require.NoError(t, json.Unmarshal(raw, &response))
	require.NotNil(t, response.Workflow.DeletedAt)
	action["expected_event_uid"] = response.Workflow.DefinitionEventUID
	status, raw = cronHTTP(t, env, http.MethodPost, path+"/restore", action)
	require.Equalf(t, 200, status, "%s", raw)
	require.NotContains(t, string(raw), `"deleted_at"`)
	response.Workflow = db.CronWorkflow{}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Nil(t, response.Workflow.DeletedAt)
}

func TestCronDefinitionWritePolicy(t *testing.T) {
	for _, kind := range []string{"readonly", "bootstrap", "enrollment", "identity"} {
		t.Run(kind, func(t *testing.T) {
			options := []testenv.Option{testenv.WithAuthToken("tok")}
			if kind == "readonly" {
				options = []testenv.Option{testenv.WithInsecureReadonly()}
			}
			if kind == "bootstrap" || kind == "identity" {
				options = append(options, testenv.WithRequireTokenIdentity())
			}
			env := testenv.New(t, options...)
			project := seedProject(t, env, "cron-policy")
			token := "tok"
			want := http.StatusForbidden
			if kind == "readonly" {
				token = ""
				want = http.StatusUnauthorized
			}
			if kind == "enrollment" {
				_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "operator")
				require.NoError(t, err)
				enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "pull,push")
				token = enrollment.Token
			}
			if kind == "identity" {
				_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "example-identity-token", Actor: "credential-worker", AdminActor: db.BootstrapActor})
				require.NoError(t, err)
				token = "example-identity-token"
				want = http.StatusCreated
			}
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID)
			headers := map[string]string{}
			if token != "" {
				headers["Authorization"] = "Bearer " + token
			}
			response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "supplied-worker", "name": "Inspect", "definition": cronHTTPDefinition(t)}, headers)
			require.Equalf(t, want, response.StatusCode, "%s", raw)
			if kind == "identity" {
				var value struct {
					Job    db.CronJob `json:"job"`
					Events []db.Event `json:"events"`
				}
				require.NoError(t, json.Unmarshal(raw, &value))
				require.Equal(t, "credential-worker", value.Job.Author)
				for _, event := range value.Events {
					require.Equal(t, "credential-worker", event.Actor)
				}
			} else {
				after, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
		})
	}
}
