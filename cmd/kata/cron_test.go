package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func cronCLIUID(t *testing.T) string {
	t.Helper()
	value, err := uid.New()
	require.NoError(t, err)
	return value
}
func cronCLIFile(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	return file
}
func executeCronCLI(t *testing.T, base string, stdin io.Reader, args ...string) (string, string, error) {
	t.Helper()
	resetFlags(t)
	command := newRootCmd()
	var out, diagnostic bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&diagnostic)
	command.SetIn(stdin)
	command.SetContext(contextWithBaseURL(context.Background(), base))
	command.SetArgs(append([]string{"--project", "spoke-project", "--json", "cron"}, args...))
	err := command.Execute()
	return out.String(), diagnostic.String(), err
}
func cronCLIJob(_ *testing.T) api.PutCronJobBody {
	return api.PutCronJobBody{Name: "Inspect", Definition: api.CronJobDefinition{Version: 1, Kind: "job", Enabled: false, Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect the workspace"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Inspection"}, Overlap: "forbid", Catchup: "all", Options: api.JSONRawObject(`{"limit":9007199254740993}`)}}
}

func TestCronCLIDefinitionsPreserveOpaqueNumbers(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	jobUID := cronCLIUID(t)
	input := cronCLIJob(t)
	file := cronCLIFile(t, input)
	out, _, err := executeCronCLI(t, env.URL, nil, "job", "create", "--uid", strings.ToLower(jobUID), "--file", file)
	require.NoError(t, err)
	require.Contains(t, out, "9007199254740993")
	var created struct {
		Job api.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	require.Equal(t, jobUID, created.Job.UID)
	out, _, err = executeCronCLI(t, env.URL, nil, "job", "list")
	require.NoError(t, err)
	require.Contains(t, out, jobUID)
	_, _, err = executeCronCLI(t, env.URL, nil, "job", "create", "--uid", jobUID, "--file", file)
	require.Error(t, err, "a stable create retry must expose 409 rather than classify every conflict as success")
	out, _, err = executeCronCLI(t, env.URL, nil, "job", "show", jobUID)
	require.NoError(t, err)
	require.Contains(t, out, "9007199254740993")
}

func TestCronCLIDefinitionRevisionAndWorkflowCRUD(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	workflowUID := cronCLIUID(t)
	input := api.PutCronWorkflowBody{Name: "Inspection", Definition: api.CronWorkflowDefinition{Version: 1, Steps: []api.CronWorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}}
	file := cronCLIFile(t, input)
	out, _, err := executeCronCLI(t, env.URL, nil, "workflow", "create", "--uid", workflowUID, "--file", file)
	require.NoError(t, err)
	var created struct {
		Workflow api.CronWorkflow `json:"workflow"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	input.Name = "Updated inspection"
	input.ExpectedEventUID = created.Workflow.DefinitionEventUID
	out, _, err = executeCronCLI(t, env.URL, nil, "workflow", "update", workflowUID, "--file", cronCLIFile(t, input))
	require.NoError(t, err)
	var updated struct {
		Workflow api.CronWorkflow `json:"workflow"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &updated))
	_, _, err = executeCronCLI(t, env.URL, nil, "workflow", "delete", workflowUID, "--expected-event-uid", created.Workflow.DefinitionEventUID)
	require.Error(t, err)
	out, _, err = executeCronCLI(t, env.URL, nil, "workflow", "delete", workflowUID, "--expected-event-uid", updated.Workflow.DefinitionEventUID)
	require.NoError(t, err)
	var deleted struct {
		Workflow api.CronWorkflow `json:"workflow"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &deleted))
	require.NotNil(t, deleted.Workflow.DeletedAt)
	out, _, err = executeCronCLI(t, env.URL, nil, "workflow", "list", "--include-deleted")
	require.NoError(t, err)
	require.Contains(t, out, workflowUID)
	out, _, err = executeCronCLI(t, env.URL, nil, "workflow", "restore", workflowUID, "--expected-event-uid", deleted.Workflow.DefinitionEventUID)
	require.NoError(t, err)
	var restored struct {
		Workflow api.CronWorkflow `json:"workflow"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &restored))
	require.Nil(t, restored.Workflow.DeletedAt)
}

func TestCronCLIRejectsInvalidInputBeforeNetwork(t *testing.T) {
	for _, tc := range []struct {
		args []string
		body string
		want string
	}{
		{[]string{"job", "create", "--file", "-"}, `{"operator":true}`, "unknown"},

		{[]string{"job", "create", "--file", "-"}, `{"actor":"one","actor":"two"}`, "duplicate"},
		{[]string{"job", "update", "01J00000000000000000000001", "--file", "-"}, `{"name":"Inspection"}`, "expected_event_uid"},
		{[]string{"run", "list", "--limit", "101"}, ``, "limit"},
		{[]string{"run", "list", "--before-uid", "bad"}, ``, "ULID"},
		{[]string{"job", "create", "--uid", "bad", "--file", "-"}, `{}`, "ULID"},
	} {
		t.Run(tc.want+strings.Join(tc.args, "-"), func(t *testing.T) {
			_, _, err := executeCronCLI(t, "http://127.0.0.1:1", strings.NewReader(tc.body), tc.args...)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestCronCLIStableCreateAfterAcceptedResponseLoss(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	var lost atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, err := http.NewRequestWithContext(r.Context(), r.Method, env.URL+r.URL.RequestURI(), r.Body) //nolint:gosec // Destination is the owned test server; only its relative request path varies.
		require.NoError(t, err)
		request.Header = r.Header.Clone()
		response, err := env.HTTP.Do(request) //nolint:gosec // Destination is the owned test server; only its relative request path varies.
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		raw, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		if r.Method == http.MethodPut && lost.CompareAndSwap(false, true) {
			require.Equal(t, 200, response.StatusCode, string(raw))
			connection, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			require.NoError(t, connection.Close())
			return
		}
		maps.Copy(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(raw)
	}))
	defer proxy.Close()
	input := cronCLIJob(t)
	file := cronCLIFile(t, input)
	_, diagnostic, err := executeCronCLI(t, proxy.URL, nil, "job", "create", "--file", file)
	require.Error(t, err)
	require.True(t, lost.Load())
	words := strings.Fields(diagnostic)
	require.GreaterOrEqual(t, len(words), 3)
	require.Equal(t, "Definition", words[0])
	retained := words[2]
	require.True(t, uid.Valid(retained))
	_, _, err = executeCronCLI(t, proxy.URL, nil, "job", "create", "--uid", retained, "--file", file)
	require.Error(t, err)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before+1, after, "retry must not emit another definition")
	values, err := env.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: project.ID, IncludeDeleted: true})
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, retained, values[0].UID)
	out, _, err := executeCronCLI(t, proxy.URL, nil, "job", "show", retained)
	require.NoError(t, err)
	var readback struct {
		Job api.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &readback))
	require.Equal(t, input.Name, readback.Job.Name)
	require.Equal(t, input.Definition, readback.Job.Definition)
	input.Name = "Later conflicting edit"
	input.ExpectedEventUID = readback.Job.DefinitionEventUID
	out, _, err = executeCronCLI(t, proxy.URL, nil, "job", "update", retained, "--file", cronCLIFile(t, input))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &readback))
	_, _, err = executeCronCLI(t, proxy.URL, nil, "job", "create", "--uid", retained, "--file", file)
	require.Error(t, err, "a conflicting later definition is not successful import recovery")
	out, _, err = executeCronCLI(t, proxy.URL, nil, "job", "show", retained)
	require.NoError(t, err)
	require.Contains(t, out, input.Name)
	_, _, err = executeCronCLI(t, proxy.URL, nil, "job", "delete", retained, "--expected-event-uid", readback.Job.DefinitionEventUID)
	require.NoError(t, err)
	_, _, err = executeCronCLI(t, proxy.URL, nil, "job", "create", "--uid", retained, "--file", file)
	require.Error(t, err, "a tombstone is not successful import recovery")
	out, _, err = executeCronCLI(t, proxy.URL, nil, "job", "show", retained)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &readback))
	require.NotNil(t, readback.Job.DeletedAt)
}

func TestCronCLICapabilitiesEmptyProject(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	before, err := env.DB.UIEventCursor(t.Context())
	require.NoError(t, err)
	out, _, err := executeCronCLI(t, env.URL, nil, "capabilities")
	require.NoError(t, err)
	require.Contains(t, out, `"cron_v1"`)
	require.Contains(t, out, project.UID)
	after, err := env.DB.UIEventCursor(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	for _, table := range []string{"cron_jobs", "cron_workflows", "cron_runs"} {
		var count int
		require.NoError(t, env.DB.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count)
	}
}
func TestCronCLICapabilitiesRejectsIssueScopedCredential(t *testing.T) {
	env := testenv.New(t, testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Delegated work", Author: "coordinator"})
	require.NoError(t, err)
	expiry := time.Now().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "scoped-reader-token", Actor: "reader", AdminActor: db.BootstrapActor, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID}, ExpiresAt: &expiry})
	require.NoError(t, err)
	t.Setenv("KATA_AUTH_TOKEN", "scoped-reader-token")
	out, _, err := executeCronCLI(t, env.URL, nil, "capabilities")
	require.Error(t, err)
	require.Empty(t, out)
}

func TestCronCLICapabilitiesAdvertisementsAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		status       int
		want         []string
	}{
		{"native", "cron_v1", 200, []string{"cron_v1"}},
		{"absent", "", 200, []string{}},
		{"unknown", "future_v2", 200, []string{"future_v2"}},
		{"csv", "future_v2, cron_v1", 200, []string{"future_v2", "cron_v1"}},
		{"unauthorized", "cron_v1", 401, nil},
		{"forbidden", "cron_v1", 403, nil},
		{"unsupported", "cron_v1", 404, nil},
		{"unavailable", "cron_v1", 503, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/projects" {
					_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`) // The owned HTTP fixture may be closed by transport-failure tests.
					return
				}
				if r.URL.Path == "/api/v1/projects/7" {
					_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`) // The owned HTTP fixture may be closed by transport-failure tests.
					return
				}
				require.Equal(t, "/api/v1/projects/7/cron/capabilities", r.URL.Path)
				w.Header().Set(db.EventFeaturesHeader, tc.header)
				w.WriteHeader(tc.status)
				if tc.status != 200 {
					_, _ = io.WriteString(w, `{"error":{"code":"unavailable","message":"discovery refused"}}`) // The owned HTTP fixture may be closed by transport-failure tests.
					return
				}
				_, _ = io.WriteString(w, `{"project_uid":"01J00000000000000000000001","event_features":["misleading_body_feature"],"opaque":{"counter":9007199254740993}}`) // The owned HTTP fixture may be closed by transport-failure tests.
			}))
			defer server.Close()
			out, _, err := executeCronCLI(t, server.URL, nil, "capabilities")
			if tc.status != 200 {
				require.Error(t, err)
				require.Empty(t, out)
				return
			}
			require.NoError(t, err)
			var response struct {
				Features []string       `json:"event_features"`
				Opaque   jsontext.Value `json:"opaque"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &response))
			require.NotNil(t, response.Features)
			require.Equal(t, tc.want, response.Features)
			require.Equal(t, `{"counter":9007199254740993}`, string(response.Opaque))
		})
	}
}
func TestCronCLICapabilitiesTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/projects" {
			_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`) // The owned HTTP fixture may be closed by transport-failure tests.
			return
		}
		if r.URL.Path == "/api/v1/projects/7" {
			_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`) // The owned HTTP fixture may be closed by transport-failure tests.
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	}))
	defer server.Close()
	out, _, err := executeCronCLI(t, server.URL, nil, "capabilities")
	require.Error(t, err)
	require.Empty(t, out)
}

func TestCronCLIIndependentRunObservations(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	definition := cronCLIJob(t).Definition.Native()
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Definition: definition, Actor: "worker"})
	require.NoError(t, err)
	occurrence := "daily:2026-10-06"
	body := api.ObserveCronRunBody{JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, OccurrenceKey: &occurrence, Status: "running", Summary: cron.Summary{Version: 1}}
	ids := []string{cronCLIUID(t), cronCLIUID(t)}
	for _, id := range ids {
		for attempt := range 2 {
			raw, _, err := executeCronCLI(t, env.URL, strings.NewReader(string(mustCronJSON(t, body))), "run", "observe", id, "--json-input", "-")
			require.NoError(t, err)
			var result struct {
				Run      api.CronRun `json:"run"`
				Replayed bool        `json:"replayed"`
			}
			require.NoError(t, json.Unmarshal([]byte(raw), &result))
			require.Equal(t, id, result.Run.UID)
			require.Equal(t, attempt == 1, result.Replayed)
		}
	}
	raw, _, err := executeCronCLI(t, env.URL, nil, "run", "list", "--limit", "1")
	require.NoError(t, err)
	require.Contains(t, raw, "next_before_uid")
	other, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Other", Definition: definition, Actor: "worker"})
	require.NoError(t, err)
	_, err = env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: cronCLIUID(t), JobUID: &other.UID, DefinitionEventUID: &other.DefinitionEventUID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	runs, err := env.DB.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, runs, 3)
	raw, _, err = executeCronCLI(t, env.URL, nil, "run", "list", "--job-uid", strings.ToLower(job.UID))
	require.NoError(t, err)
	var page struct {
		Runs []api.CronRun `json:"runs"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &page))
	listed := []string{}
	for _, run := range page.Runs {
		listed = append(listed, run.UID)
	}
	require.ElementsMatch(t, ids, listed, "--job-uid lists only that job's runs")
}
func mustCronJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

func TestCronCLIObservationTeammateSelection(t *testing.T) {
	for _, tc := range []struct {
		name, env, body string
		flag            []string
		want            string
		invalid         bool
	}{
		{name: "flag", flag: []string{"--teammate", "adapter"}, want: "adapter"},
		{name: "environment", env: "adapter", want: "adapter"},
		{name: "empty flag suppresses environment", env: "adapter", flag: []string{"--teammate", ""}},
		{name: "frozen body ignores environment", env: "another", body: `"frozen"`, want: "frozen"},
		{name: "frozen null ignores environment", env: "another", body: `null`},
		{name: "matching flag", env: "another", body: `"frozen"`, flag: []string{"--teammate", "frozen"}, want: "frozen"},
		{name: "matching empty flag", env: "another", body: `null`, flag: []string{"--teammate", ""}},
		{name: "conflicting flag", body: `"frozen"`, flag: []string{"--teammate", "another"}, invalid: true},
		{name: "empty flag conflicts with frozen body", body: `"frozen"`, flag: []string{"--teammate", ""}, invalid: true},
		{name: "flag conflicts with frozen null", body: `null`, flag: []string{"--teammate", "adapter"}, invalid: true},
		{name: "empty body retains bounds", env: "adapter", body: `""`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KATA_AUTHOR", "worker")
			t.Setenv("KATA_TEAMMATE", tc.env)
			env := testenv.New(t)
			project, err := env.DB.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Definition: cronCLIJob(t).Definition.Native(), Actor: "worker"})
			require.NoError(t, err)
			fields := map[string]jsontext.Value{}
			require.NoError(t, json.Unmarshal(mustCronJSON(t, api.ObserveCronRunBody{JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1}}), &fields))
			if tc.body != "" {
				fields["teammate"] = []byte(tc.body)
			}
			id := cronCLIUID(t)
			args := append([]string{"run", "observe", id, "--json-input", "-"}, tc.flag...)
			out, _, err := executeCronCLI(t, env.URL, strings.NewReader(string(mustCronJSON(t, fields))), args...)
			if tc.invalid {
				require.Error(t, err)
				_, err := env.DB.CronRun(t.Context(), project.ID, id)
				require.ErrorIs(t, err, db.ErrNotFound, "conflicting identity cannot write")
				return
			}
			require.NoError(t, err)
			var observed struct {
				Run      api.CronRun `json:"run"`
				Replayed bool        `json:"replayed"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &observed))
			if tc.want == "" {
				require.Nil(t, observed.Run.Teammate)
				fields["teammate"] = []byte(`null`)
			} else {
				require.Equal(t, &tc.want, observed.Run.Teammate)
				fields["teammate"] = mustCronJSON(t, tc.want)
			}
			cursor, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			t.Setenv("KATA_TEAMMATE", "changed-environment")
			out, _, err = executeCronCLI(t, env.URL, strings.NewReader(string(mustCronJSON(t, fields))), "run", "observe", id, "--json-input", "-")
			require.NoError(t, err, "retry uses frozen attribution")
			require.NoError(t, json.Unmarshal([]byte(out), &observed))
			require.True(t, observed.Replayed)
			after, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, cursor, after, "retry emits no event")
		})
	}
}

type cronCLIState struct {
	maxEventID int64
	jobs       []db.CronJob
	workflows  []db.CronWorkflow
	runs       []db.CronRun
}

func readCronCLIState(t *testing.T, store db.Storage, projectID int64) cronCLIState {
	t.Helper()
	var state cronCLIState
	var err error
	state.maxEventID, err = store.MaxEventID(t.Context())
	require.NoError(t, err)
	state.jobs, err = store.ListCronJobs(t.Context(), db.CronList{ProjectID: projectID, IncludeDeleted: true})
	require.NoError(t, err)
	state.workflows, err = store.ListCronWorkflows(t.Context(), db.CronList{ProjectID: projectID, IncludeDeleted: true})
	require.NoError(t, err)
	state.runs, err = store.ListCronRuns(t.Context(), db.CronRunList{ProjectID: projectID})
	require.NoError(t, err)
	return state
}

// Malformed CLI requests fail as validation errors before they reach the
// daemon, so no cron definition, run or event is written.
func TestCronCLIRequestValidationLeavesStateUnchanged(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Definition: cronCLIJob(t).Definition.Native(), Actor: "worker"})
	require.NoError(t, err)
	createWithEvent := cronCLIJob(t)
	createWithEvent.ExpectedEventUID = job.DefinitionEventUID
	observe := api.ObserveCronRunBody{JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1}}
	numericTeammate := map[string]jsontext.Value{}
	require.NoError(t, json.Unmarshal(mustCronJSON(t, observe), &numericTeammate))
	numericTeammate["teammate"] = []byte(`5`)
	runUID := cronCLIUID(t)
	for _, tc := range []struct {
		name string
		args []string
		body string
		want string
	}{
		{
			name: "create with expected event",
			args: []string{"job", "create", "--file", "-"},
			body: string(mustCronJSON(t, createWithEvent)),
			want: "create requires absent expected_event_uid",
		},
		{
			name: "body and flag events differ",
			args: []string{"job", "delete", job.UID, "--expected-event-uid", job.DefinitionEventUID, "--file", "-"},
			body: `{"expected_event_uid":"` + cronCLIUID(t) + `"}`,
			want: "expected_event_uid in body and flag must match",
		},
		{
			name: "body actor differs from --as",
			args: []string{"--as", "other", "job", "delete", job.UID, "--expected-event-uid", job.DefinitionEventUID, "--file", "-"},
			body: `{"actor":"worker"}`,
			want: "body actor and --as must match",
		},
		{name: "definition without file", args: []string{"job", "create"}, want: "--file is required"},
		{name: "observation without input", args: []string{"run", "observe", runUID}, want: "--json-input is required"},
		{
			name: "oversized definition",
			args: []string{"job", "create", "--file", "-"},
			body: strings.Repeat(" ", 1<<20+1),
			want: "cron request exceeds its byte limit",
		},
		{
			name: "oversized observation",
			args: []string{"run", "observe", runUID, "--json-input", "-"},
			body: strings.Repeat(" ", db.CronObservationLimit+1),
			want: "cron request exceeds its byte limit",
		},
		{
			name: "numeric teammate with flag",
			args: []string{"run", "observe", runUID, "--json-input", "-", "--teammate", "adapter"},
			body: string(mustCronJSON(t, numericTeammate)),
			want: "invalid cron request",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := readCronCLIState(t, env.DB, project.ID)
			out, _, err := executeCronCLI(t, env.URL, strings.NewReader(tc.body), tc.args...)
			var cliErr *cliError
			require.ErrorAs(t, err, &cliErr)
			require.Equal(t, kindValidation, cliErr.Kind)
			require.Equal(t, ExitValidation, cliErr.ExitCode)
			require.Contains(t, cliErr.Message, tc.want)
			require.Empty(t, out)
			require.Equal(t, before, readCronCLIState(t, env.DB, project.ID))
		})
	}
}
