package daemon_test

import (
	"encoding/json/jsontext"
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

// The status clock is explicit so the boundary is independent of wall time.
func deliveryHTTPFixture(t *testing.T) (*testenv.Env, db.Project, db.Issue, db.CronJob, time.Time) {
	t.Helper()
	now := time.Now().UTC().Add(5 * time.Hour)
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) { cfg.UIClock = func() time.Time { return now } })
	p := seedProject(t, env, "delivery-example")
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Recurring parent", Author: "worker"})
	require.NoError(t, err)
	j, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Check service", Actor: "worker", Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Check service"}, Overlap: "allow", Catchup: "latest"}})
	require.NoError(t, err)
	parent, err = env.DB.IssueByID(t.Context(), parent.ID)
	require.NoError(t, err)
	return env, p, parent, j, now
}

func TestCronJobShowIncludesDelivery(t *testing.T) {
	env, p, _, j, now := deliveryHTTPFixture(t)
	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s?include=unknown,delivery", p.ID, j.UID), nil)
	require.Equalf(t, 200, status, "%s", raw)
	var out struct {
		Job struct {
			Delivery struct {
				State       string    `json:"state"`
				Next        string    `json:"next_due_at"`
				EvaluatedAt time.Time `json:"evaluated_at"`
			} `json:"delivery"`
		} `json:"job"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, "behind", out.Job.Delivery.State)
	require.NotEmpty(t, out.Job.Delivery.Next)
	require.Equal(t, now, out.Job.Delivery.EvaluatedAt)
}

func TestCronJobListDeliveryOnlyForNativeJobs(t *testing.T) {
	env, p, _, j, _ := deliveryHTTPFixture(t)
	_, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Adapter", Actor: "worker", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs?include=delivery", p.ID), nil)
	require.Equalf(t, 200, status, "%s", raw)
	var out struct {
		Jobs []map[string]jsontext.Value `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Jobs, 2)
	for _, job := range out.Jobs {
		if string(job["uid"]) == `"`+j.UID+`"` {
			require.Contains(t, job, "delivery")
		} else {
			require.NotContains(t, job, "delivery")
		}
	}
}

func TestCronDeliveryReadsDoNotWrite(t *testing.T) {
	env, p, parent, j, _ := deliveryHTTPFixture(t)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	children, err := env.DB.ChildrenOfIssue(t.Context(), parent.ID)
	require.NoError(t, err)
	for range 3 {
		status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s?include=delivery", p.ID, j.UID), nil)
		require.Equalf(t, 200, status, "%s", raw)
		require.Contains(t, string(raw), `"delivery"`)
	}
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	current, err := env.DB.IssueByID(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Equal(t, parent.Metadata, current.Metadata)
	require.Equal(t, parent.Revision, current.Revision)
	currentChildren, err := env.DB.ChildrenOfIssue(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, currentChildren, len(children))
}

func TestCronJobResponsesOmitDeliveryByDefault(t *testing.T) {
	env, p, _, j, _ := deliveryHTTPFixture(t)
	base := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", p.ID)
	path := base + "/" + j.UID
	for _, url := range []string{base, path, path + "?include=unknown"} {
		status, raw := cronHTTP(t, env, http.MethodGet, url, nil)
		require.Equalf(t, 200, status, "%s", raw)
		require.NotContains(t, string(raw), `"delivery"`)
	}
	body := map[string]any{"actor": "worker", "name": j.Name, "definition": j.Definition, "expected_event_uid": j.DefinitionEventUID}
	status, raw := cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "%s", raw)
	require.NotContains(t, string(raw), `"delivery"`)
	var updated struct {
		Job db.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal(raw, &updated))
	body["name"] = "Stale change"
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equal(t, 409, status)
	require.NotContains(t, string(raw), `"delivery"`)
	action := map[string]any{"actor": "worker", "expected_event_uid": updated.Job.DefinitionEventUID}
	status, raw = cronHTTP(t, env, http.MethodDelete, path, action)
	require.Equalf(t, 200, status, "%s", raw)
	require.NotContains(t, string(raw), `"delivery"`)
	require.NoError(t, json.Unmarshal(raw, &updated))
	action["expected_event_uid"] = updated.Job.DefinitionEventUID
	status, raw = cronHTTP(t, env, http.MethodPost, path+"/restore", action)
	require.Equalf(t, 200, status, "%s", raw)
	require.NotContains(t, string(raw), `"delivery"`)
	status, raw = cronHTTP(t, env, http.MethodPost, base, map[string]any{"actor": "worker", "name": "Adapter", "definition": cronHTTPDefinition(t)})
	require.Equalf(t, 201, status, "%s", raw)
	require.NotContains(t, string(raw), `"delivery"`)
}
