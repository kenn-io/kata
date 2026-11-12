package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
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

func TestCronNativePushDisabledSpokeWriteHint(t *testing.T) {
	for _, operation := range []string{"create", "update", "delete", "restore"} {
		t.Run(operation, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			project := seedProject(t, env, "spoke-project")
			parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Periodic work", Author: "operator"})
			require.NoError(t, err)
			definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true,
				Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200},
				Action:  cron.Action{Kind: "notify", Recipient: "current-owner-or-author"},
				Issue:   &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review service"}, Overlap: "allow", Catchup: "all"}
			path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID)
			method := http.MethodPost
			body := map[string]any{"actor": "operator", "name": "Periodic review", "definition": definition}
			if operation != "create" {
				job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Periodic review", Actor: "operator", Definition: definition})
				require.NoError(t, err)
				if operation == "restore" {
					job, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Definition: job.Definition, Actor: "operator", Deleted: true})
					require.NoError(t, err)
				}
				body["expected_event_uid"] = job.DefinitionEventUID
				path += "/" + job.UID
				switch operation {
				case "update":
					method = http.MethodPut
				case "delete":
					method = http.MethodDelete
					delete(body, "name")
					delete(body, "definition")
				case "restore":
					path += "/restore"
					delete(body, "name")
					delete(body, "definition")
				}
			}
			binding, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID,
				Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42,
				HubProjectUID: project.UID, Enabled: true, PushEnabled: false, Actor: "operator"})
			require.NoError(t, err)
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			status, raw := cronHTTP(t, env, method, path, body)
			require.Equalf(t, http.StatusConflict, status, "%s", raw)
			var response api.ErrorEnvelope
			require.NoError(t, json.Unmarshal(raw, &response))
			require.Equal(t, "federated_read_only", response.Error.Code)
			require.Equal(t, "native recurring jobs are created and edited on the project's owning hub", response.Error.Message)
			require.Equal(t, "Run the same command against the project's owning hub, for example with --daemon <hub-name>.", response.Error.Hint)
			after, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
			// Ordinary adapter jobs remain writable on push-enabled spokes.
			binding.PushEnabled = true
			_, err = env.DB.UpsertFederationBinding(t.Context(), binding)
			require.NoError(t, err)
			status, raw = cronHTTP(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID),
				map[string]any{"actor": "operator", "name": "Adapter job", "definition": cronHTTPDefinition(t)})
			require.Equalf(t, http.StatusCreated, status, "%s", raw)
		})
	}
}

func TestCronNativeHubCreate(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			project := seedProject(t, env, "hub-project")
			parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Periodic work", Author: "operator"})
			require.NoError(t, err)
			binding, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "operator")
			require.NoError(t, err)
			binding.Enabled = enabled
			_, err = env.DB.UpsertFederationBinding(t.Context(), binding)
			require.NoError(t, err)
			definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true,
				Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200},
				Action:  cron.Action{Kind: "notify", Recipient: "current-owner-or-author"},
				Issue:   &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review service"}, Overlap: "allow", Catchup: "all"}
			status, raw := cronHTTP(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID),
				map[string]any{"actor": "operator", "name": "Periodic review", "definition": definition})
			if enabled {
				require.Equalf(t, http.StatusCreated, status, "%s", raw)
			} else {
				require.Equalf(t, http.StatusConflict, status, "%s", raw)
				var response api.ErrorEnvelope
				require.NoError(t, json.Unmarshal(raw, &response))
				require.Equal(t, "federated_read_only", response.Error.Code)
				require.Equal(t, "federated spoke project is read-only\nfederated spoke operation unsupported", response.Error.Message)
				require.Empty(t, response.Error.Hint)
			}
		})
	}
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

func TestCronInputWorkflowChangeAndProjectScope(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	{
		path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID)
		definition := cronHTTPDefinition(t)
		definition.Options = []byte(`{"inputs":{"batch":{"type":"int","min":1,"max":5}},"input_values":{"batch":8}}`)
		body := map[string]any{"actor": "worker", "name": "Inspect", "definition": definition}
		status, raw := cronHTTP(t, env, http.MethodPost, path, body)
		require.Equalf(t, 400, status, "%s", raw)
		definition.Options = []byte(`{"inputs":{"batch":{"type":"int","min":1,"max":5},"empty_default":{"type":"string","default":""},"empty_fixed":{"type":"string"},"flag":{"type":"bool","default":true},"unset":{"type":"string"}},"input_values":{"batch":3,"empty_fixed":""}}`)
		body["definition"] = definition
		status, raw = cronHTTP(t, env, http.MethodPost, path, body)
		require.Equalf(t, 201, status, "%s", raw)
		var response struct {
			Job db.CronJob `json:"job"`
		}
		require.NoError(t, json.Unmarshal(raw, &response))
		status, raw = cronHTTP(t, env, http.MethodGet, path+"/"+response.Job.UID+"/inputs", nil)
		require.Equalf(t, 200, status, "%s", raw)
		var resolved struct {
			DeclaredBy string           `json:"declared_by"`
			Inputs     []map[string]any `json:"inputs"`
		}
		require.NoError(t, json.Unmarshal(raw, &resolved))
		require.Equal(t, "job", resolved.DeclaredBy)
		require.Len(t, resolved.Inputs, 5)
		require.Equal(t, float64(3), resolved.Inputs[0]["value"])
		require.Equal(t, "job", resolved.Inputs[0]["source"])
		require.Equal(t, "", resolved.Inputs[1]["value"])
		require.Equal(t, "default", resolved.Inputs[1]["source"])
		require.Equal(t, "", resolved.Inputs[2]["value"])
		require.Equal(t, "job", resolved.Inputs[2]["source"])
		require.Equal(t, true, resolved.Inputs[3]["value"])
		require.NotContains(t, resolved.Inputs[4], "value")
	}
	other := seedProject(t, env, "other-project")
	workflows := fmt.Sprintf("/api/v1/projects/%d/cron/workflows", project.ID)
	definition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}, Options: []byte(`{"inputs":{"target":{"type":"enum","values":["staging","production"]}}}`)}
	body := map[string]any{"actor": "worker", "name": "Inspect", "definition": definition}
	status, raw := cronHTTP(t, env, http.MethodPost, workflows, body)
	require.Equalf(t, 201, status, "%s", raw)
	var created struct {
		Workflow db.CronWorkflow `json:"workflow"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	job := cronHTTPDefinition(t)
	job.Action = cron.Action{Kind: "execute", WorkflowUID: created.Workflow.UID}
	job.Options = []byte(`{"inputs":{"target":{"type":"bool"}}}`)
	jobs := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID)
	status, _ = cronHTTP(t, env, http.MethodPost, jobs, map[string]any{"actor": "worker", "name": "Inspect", "definition": job})
	require.Equal(t, 400, status)
	job.Options = []byte(`{"input_values":{"target":"production"}}`)
	status, raw = cronHTTP(t, env, http.MethodPost, jobs, map[string]any{"actor": "worker", "name": "Inspect", "definition": job})
	require.Equalf(t, 201, status, "%s", raw)
	var result struct {
		Job db.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	definition.Options = []byte(`{"inputs":{"target":{"type":"enum","values":["staging"]}}}`)
	body["definition"] = definition
	body["expected_event_uid"] = created.Workflow.DefinitionEventUID
	status, raw = cronHTTP(t, env, http.MethodPut, workflows+"/"+created.Workflow.UID, body)
	require.Equalf(t, 200, status, "%s", raw)
	status, raw = cronHTTP(t, env, http.MethodGet, jobs+"/"+result.Job.UID+"/inputs", nil)
	require.Equalf(t, 200, status, "%s", raw)
	require.Contains(t, string(raw), "invalid_value")
	job.Enabled = false
	status, _ = cronHTTP(t, env, http.MethodPut, jobs+"/"+result.Job.UID, map[string]any{"actor": "worker", "name": "Inspect", "definition": job, "expected_event_uid": result.Job.DefinitionEventUID})
	require.Equal(t, 400, status)
	action := map[string]any{"actor": "worker", "expected_event_uid": result.Job.DefinitionEventUID}
	status, raw = cronHTTP(t, env, http.MethodDelete, jobs+"/"+result.Job.UID, action)
	require.Equalf(t, 200, status, "%s", raw)
	require.NoError(t, json.Unmarshal(raw, &result))
	action["expected_event_uid"] = result.Job.DefinitionEventUID
	status, raw = cronHTTP(t, env, http.MethodPost, jobs+"/"+result.Job.UID+"/restore", action)
	require.Equalf(t, 200, status, "%s", raw)
	status, raw = cronHTTP(t, env, http.MethodGet, jobs+"/"+result.Job.UID+"/inputs", nil)
	require.Equalf(t, 200, status, "%s", raw)
	require.Contains(t, string(raw), "invalid_value")

	status, _ = cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/inputs", other.ID, result.Job.UID), nil)
	require.Equal(t, 404, status)
}

func TestCronInputWorkflowWriteValidation(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/workflows", project.ID)
	definition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}, Options: []byte(`{"inputs":42}`)}
	body := map[string]any{"actor": "worker", "name": "Inspect", "definition": definition}
	status, _ := cronHTTP(t, env, http.MethodPost, path, body)
	require.Equal(t, 400, status)
	definition.Options = []byte(`{}`)
	body["definition"] = definition
	status, raw := cronHTTP(t, env, http.MethodPost, path, body)
	require.Equalf(t, 201, status, "%s", raw)
	var created struct {
		Workflow db.CronWorkflow `json:"workflow"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	definition.Options = []byte(`{"inputs":42}`)
	body["definition"] = definition
	body["expected_event_uid"] = created.Workflow.DefinitionEventUID
	status, _ = cronHTTP(t, env, http.MethodPut, path+"/"+created.Workflow.UID, body)
	require.Equal(t, 400, status)
}

func TestCronNotifyInputWrites(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID)
	definition := cronHTTPDefinition(t)
	definition.Action = cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Target", Author: "worker"})
	require.NoError(t, err)
	definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: issue.UID}
	body := map[string]any{"actor": "worker", "name": "Notify", "definition": definition}
	status, raw := cronHTTP(t, env, http.MethodPost, path, body)
	require.Equalf(t, 201, status, "%s", raw)
	var created struct {
		Job db.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	for _, options := range []string{`{"inputs":{"x":{"type":"bool"}}}`, `{"input_values":{"x":true}}`} {
		definition.Options = []byte(options)
		body["definition"] = definition
		status, _ = cronHTTP(t, env, http.MethodPost, path, body)
		require.Equal(t, 400, status)
		body["expected_event_uid"] = created.Job.DefinitionEventUID
		status, _ = cronHTTP(t, env, http.MethodPut, path+"/"+created.Job.UID, body)
		require.Equal(t, 400, status)
		delete(body, "expected_event_uid")
	}
}

// TestCronInputsArchivedWorkflowContract refuses resolution of a retained tombstone.
func TestCronInputsArchivedWorkflowContract(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	workflow, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "Inspect", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	definition := cronHTTPDefinition(t)
	definition.Action = cron.Action{Kind: "execute", WorkflowUID: workflow.UID}
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	_, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, Name: job.Name, Actor: "worker", Definition: definition, ExpectedEventUID: job.DefinitionEventUID, Deleted: true})
	require.NoError(t, err)
	_, _, err = env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, UID: workflow.UID, Name: workflow.Name, Actor: "worker", Definition: workflow.Definition, ExpectedEventUID: workflow.DefinitionEventUID, Deleted: true})
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/inputs", project.ID, job.UID), nil)
	require.Equalf(t, 200, status, "%s", raw)
	require.Contains(t, string(raw), "declaration_invalid")
}
