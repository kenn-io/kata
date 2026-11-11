package daemon_test

import (
	"context"
	"encoding/json/jsontext"
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
	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs?include=delivery", p.ID), nil)
	require.Equalf(t, 200, status, "%s", raw)
	var out struct {
		Jobs []map[string]jsontext.Value `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Jobs, 1)
	require.JSONEq(t, `"`+j.UID+`"`, string(out.Jobs[0]["uid"]))
	require.Contains(t, out.Jobs[0], "delivery")
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
}

// Read the actual delivery projection before committing a child move; the
// buffered response must recheck every issue that supplied delivery evidence.
type deliveryAfterReadStore struct {
	db.Storage
	afterRead func() error
}

func (s *deliveryAfterReadStore) RecurringCronDelivery(ctx context.Context, query db.CronDeliveryQuery, now time.Time) ([]db.CronDeliveryJob, error) {
	out, err := s.Storage.RecurringCronDelivery(ctx, query, now)
	if err == nil && s.afterRead != nil {
		after := s.afterRead
		s.afterRead = nil
		err = after()
	}
	return out, err
}

func TestCronDeliveryResponseRelease(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		now := time.Now().UTC().Add(5 * time.Hour)
		clock := func() time.Time { return now }
		f = projectAccessFixtureWithStorage(t, f, store, clock)
		hidden := projectAccessHiddenProject(t, store, "hidden-project")
		j, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.private.ID, Name: "Recurring work", Actor: "member", Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: f.issue.UID, Title: "Recurring work"}, Overlap: "allow", Catchup: "latest"}})
		require.NoError(t, err)
		_, err = store.DispatchRecurringCron(t.Context(), db.DispatchRecurringCronIn{ProjectID: f.private.ID, JobUID: j.UID, ExpectedEventUID: j.DefinitionEventUID, Now: now})
		require.NoError(t, err)
		children, err := store.ChildrenOfIssue(t.Context(), f.issue.ID)
		require.NoError(t, err)
		require.Len(t, children, 1)
		delivery, err := store.RecurringCronDelivery(t.Context(), db.CronDeliveryQuery{ProjectID: f.private.ID, JobUID: j.UID}, now)
		require.NoError(t, err)
		selected := delivery[0].Delivery.LastChild.UID
		var child db.Issue
		for _, candidate := range children {
			if candidate.UID == selected {
				child = candidate
			}
		}
		path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s?include=delivery", f.private.ID, j.UID)
		status, _, raw := f.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), selected)
		moved := false
		wrapped := &deliveryAfterReadStore{Storage: store, afterRead: func() error {
			_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: child.ID, FromProjectID: f.private.ID, ToProjectID: hidden.ID, IfMatchRev: child.Revision, Actor: "member"})
			moved = err == nil
			return err
		}}
		f = projectAccessFixtureWithStorage(t, f, wrapped, clock)
		status, _, raw = f.request(t, http.MethodGet, path, "member", nil, nil)
		require.True(t, moved)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		require.NotContains(t, string(raw), selected)
	})
}
