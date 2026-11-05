package daemon_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func executionBinding(t *testing.T, store db.Storage, project db.Project, role db.FederationRole, enabled, relay bool) {
	t.Helper()
	_, err := store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: role, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", Enabled: enabled, PushEnabled: true})
	require.NoError(t, err)
	if relay {
		configureClaimRelayBinding(t, store, project, cronHTTPUID(t), "company-member", "local-member")
	}
}

func TestCronExecutionCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    db.FederationRole
		enabled bool
		cfg     config.CronConfig
		want    api.CronExecutionPolicy
	}{
		{name: "local", want: api.CronExecutionPolicy{Mode: "all", Source: "local"}},
		{name: "hub", role: db.FederationRoleHub, want: api.CronExecutionPolicy{Mode: "all", Source: "local"}},
		{name: "direct", role: db.FederationRoleSpoke, enabled: true, want: api.CronExecutionPolicy{Replica: true, Mode: "none", MemberActor: "company-member", Source: "replica_default"}},
		{name: "disabled", role: db.FederationRoleSpoke, want: api.CronExecutionPolicy{Replica: true, Mode: "none", MemberActor: "company-member", Source: "replica_default"}},
		{name: "owned default", role: db.FederationRoleSpoke, enabled: true, cfg: config.CronConfig{ReplicaExecute: "owned"}, want: api.CronExecutionPolicy{Replica: true, Mode: "owned", MemberActor: "company-member", Source: "replica_default"}},
		{name: "override", role: db.FederationRoleSpoke, enabled: true, cfg: config.CronConfig{ReplicaExecute: "all", Projects: []config.CronProjectConfig{{Name: "company-project", Execute: "owned"}}}, want: api.CronExecutionPolicy{Replica: true, Mode: "owned", MemberActor: "company-member", Source: "project"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CronExecution = tc.cfg })
			project := seedProject(t, env, "company-project")
			if tc.role != "" {
				executionBinding(t, env.DB, project, tc.role, tc.enabled, false)
			}
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", project.ID), nil)
			require.Equalf(t, 200, status, "%s", raw)
			var body struct {
				Execution api.CronExecutionPolicy `json:"execution"`
			}
			require.NoError(t, json.Unmarshal(raw, &body))
			require.Equal(t, tc.want, body.Execution)
			after, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
			if !tc.want.Replica {
				require.NotContains(t, string(raw), "member_actor")
			}
		})
	}
}

func TestCronExecutionNewRunPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, mode, author, kind    string
		workflowAuthor, editor      string
		stale, missingJob           bool
		omittedJob, omittedWorkflow bool
		actionWorkflow              bool
		missingActionWorkflow       bool
		workflow, deleted, relay    bool
		want                        int
	}{
		{name: "none", author: "company-member", want: 403},
		{name: "owned author", mode: "owned", author: "company-member", want: 200},
		{name: "owned other", mode: "owned", author: "other-member", want: 403},
		{name: "owned case differs", mode: "owned", author: "Company-member", want: 403},
		{name: "relay upstream", mode: "owned", author: "company-member", relay: true, want: 200},
		{name: "relay local", mode: "owned", author: "local-member", relay: true, want: 403},
		{name: "all", mode: "all", author: "other-member", want: 200},
		{name: "workflow owned", mode: "owned", author: "company-member", workflow: true, want: 200},
		{name: "workflow other", mode: "owned", author: "other-member", workflow: true, want: 403},
		{name: "workflow none", author: "company-member", workflow: true, want: 403},
		{name: "stale notify", author: "other-member", kind: "notify", stale: true, want: 403},
		{name: "typo job", author: "company-member", missingJob: true, want: 403},
		{name: "omitted job none", mode: "none", author: "company-member", omittedJob: true, want: 403},
		{name: "omitted job owned", mode: "owned", author: "company-member", omittedJob: true, want: 403},
		{name: "omitted workflow none", mode: "none", author: "other-member", workflow: true, omittedWorkflow: true, want: 403},
		{name: "omitted workflow owned", mode: "owned", author: "other-member", workflow: true, omittedWorkflow: true, want: 403},
		{name: "missing job with foreign body workflow", mode: "owned", missingJob: true, workflow: true, workflowAuthor: "other-member", want: 403},
		{name: "missing job with owned body workflow", mode: "owned", missingJob: true, workflow: true, workflowAuthor: "company-member", want: 403},
		{name: "owned job after colleague edit", mode: "owned", author: "company-member", editor: "other-member", want: 200},
		{name: "owned workflow after colleague edit", mode: "owned", author: "company-member", workflow: true, editor: "other-member", want: 200},
		{name: "notify", author: "other-member", kind: "notify", want: 200},
		{name: "notify workflow", author: "other-member", kind: "notify", workflow: true, want: 403},
		{name: "execute with foreign body workflow", mode: "owned", author: "company-member", kind: "execute", workflow: true, workflowAuthor: "other-member", want: 403},
		{name: "execute with owned body workflow", mode: "owned", author: "company-member", kind: "execute", workflow: true, workflowAuthor: "company-member", want: 200},
		{name: "owned job foreign action workflow", mode: "owned", author: "company-member", actionWorkflow: true, workflowAuthor: "other-member", want: 403},
		{name: "owned job owned action workflow", mode: "owned", author: "company-member", actionWorkflow: true, workflowAuthor: "company-member", want: 200},
		{name: "owned job missing action workflow with foreign body workflow", mode: "owned", author: "company-member", actionWorkflow: true, missingActionWorkflow: true, workflow: true, workflowAuthor: "other-member", want: 403},
		{name: "tombstone", mode: "owned", author: "company-member", deleted: true, want: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CronExecution = config.CronConfig{ReplicaExecute: tc.mode} })
			project := seedProject(t, env, "company-project")
			body := map[string]any{"actor": "observer", "status": "running", "summary": map[string]any{"version": 1}, "expected_revision": 0}
			var wf db.CronWorkflow
			if tc.workflow || tc.actionWorkflow {
				author := tc.workflowAuthor
				if author == "" {
					author = tc.author
				}
				var err error
				wf, _, err = env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "Inspect", Actor: author, Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}})
				require.NoError(t, err)
				if tc.editor != "" {
					def := wf.Definition
					def.Steps[0].Command = "echo changed"
					wf, _, err = env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, UID: wf.UID, ExpectedEventUID: wf.DefinitionEventUID, Name: wf.Name, Actor: tc.editor, Definition: def})
					require.NoError(t, err)
				}
				if tc.workflow {
					body["workflow_uid"], body["workflow_definition_event_uid"] = wf.UID, wf.DefinitionEventUID
				}
			}
			if !tc.workflow || tc.kind != "" || tc.actionWorkflow {
				def := cronHTTPDefinition(t)
				if tc.actionWorkflow {
					def.Action = cron.Action{Kind: "execute", WorkflowUID: wf.UID}
				}
				if tc.kind == "notify" {
					def.Action = cron.Action{Kind: "notify", Recipient: "company-member", Message: "Inspect"}
					issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Inspect", Author: tc.author})
					require.NoError(t, err)
					def.Issue = &cron.IssuePolicy{Kind: "existing", UID: issue.UID}
				}
				job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: tc.author, Definition: def})
				require.NoError(t, err)
				if tc.missingActionWorkflow {
					// Restored definitions can reference workflows absent from this store.
					_, err = env.DB.ExecContext(t.Context(), `UPDATE cron_jobs SET definition_json = json_set(definition_json, '$.action.workflow_uid', ?) WHERE uid = ?`, cronHTTPUID(t), job.UID)
					require.NoError(t, err)
				}
				oldEvent := job.DefinitionEventUID
				if tc.stale || tc.editor != "" && !tc.actionWorkflow {
					actor := tc.editor
					if actor == "" {
						actor = tc.author
					}
					job, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: "Updated", Actor: actor, Definition: def})
					require.NoError(t, err)
				}
				if tc.deleted {
					job, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Actor: tc.author, Definition: def, Deleted: true})
					require.NoError(t, err)
				}
				body["job_uid"], body["definition_event_uid"] = job.UID, job.DefinitionEventUID
				if tc.stale {
					body["definition_event_uid"] = oldEvent
				}
			}
			if tc.missingJob {
				body["job_uid"], body["definition_event_uid"] = cronHTTPUID(t), cronHTTPUID(t)
			}
			// An export can omit archived definitions while retaining their events.
			if tc.omittedJob {
				_, err := env.DB.ExecContext(t.Context(), `DELETE FROM cron_jobs WHERE uid = ?`, body["job_uid"])
				require.NoError(t, err)
			}
			if tc.omittedWorkflow {
				_, err := env.DB.ExecContext(t.Context(), `DELETE FROM cron_workflows WHERE uid = ?`, body["workflow_uid"])
				require.NoError(t, err)
			}
			executionBinding(t, env.DB, project, db.FederationRoleSpoke, true, tc.relay)
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			runUID := cronHTTPUID(t)
			status, raw := cronHTTP(t, env, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, runUID), body)
			require.Equalf(t, tc.want, status, "%s", raw)
			if tc.want == 403 {
				var problem api.ErrorEnvelope
				require.NoError(t, json.Unmarshal(raw, &problem))
				require.Equal(t, "cron_execution_not_enabled", problem.Error.Code)
				if tc.missingJob || tc.omittedJob || tc.omittedWorkflow || tc.missingActionWorkflow {
					require.Contains(t, problem.Error.Hint, "Check the definition UID")
				} else {
					require.Contains(t, problem.Error.Hint, "[[cron.project]]")
				}
				require.Contains(t, problem.Error.Message, "replicated project")
				if tc.missingJob || tc.omittedJob || tc.omittedWorkflow {
					kind, id := "job", body["job_uid"]
					if tc.omittedWorkflow {
						kind, id = "workflow", body["workflow_uid"]
					}
					require.Contains(t, problem.Error.Message, fmt.Sprintf("%s %s", kind, id))
					require.Contains(t, problem.Error.Message, "definition was not found on this daemon")
				}
				mode := tc.mode
				if mode == "" {
					mode = "none"
				}
				require.Equal(t, mode, problem.Error.Data["mode"])
				require.Equal(t, "company-member", problem.Error.Data["member_actor"])
				after, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after)
				_, err = env.DB.CronRun(t.Context(), project.ID, runUID)
				require.ErrorIs(t, err, db.ErrNotFound)
			}
		})
	}
}

func TestCronExecutionExistingRun(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "company-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "company-member", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	id := cronHTTPUID(t)
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, id)
	body := map[string]any{"actor": "observer", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "running", "summary": map[string]any{"version": 1}, "expected_revision": 0}
	status, raw := cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "%s", raw)
	executionBinding(t, env.DB, project, db.FederationRoleSpoke, true, false)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "replay: %s", raw)
	var replay struct {
		Replayed bool `json:"replayed"`
	}
	require.NoError(t, json.Unmarshal(raw, &replay))
	require.True(t, replay.Replayed)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "replaying after opt-out creates no event")
	body["status"], body["expected_revision"] = "succeeded", 1
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "finish: %s", raw)
	finished, err := env.DB.CronRun(t.Context(), project.ID, id)
	require.NoError(t, err)
	require.Equal(t, "succeeded", finished.Status)
	require.EqualValues(t, 2, finished.Revision)
	before, err = env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	body["job_uid"] = cronHTTPUID(t)
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 409, status, "identity: %s", raw)
	after, err = env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "an identity conflict after opt-out creates no event")
	unchanged, err := env.DB.CronRun(t.Context(), project.ID, id)
	require.NoError(t, err)
	require.Equal(t, finished, unchanged)
}

func TestCronExecutionOverrideFollowsCurrentName(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.CronExecution = config.CronConfig{ReplicaExecute: "all", Projects: []config.CronProjectConfig{{Name: "company-project", Execute: "none"}, {Name: " spoke-project ", Execute: "owned"}}}
	})
	project := seedProject(t, env, "company-project")
	executionBinding(t, env.DB, project, db.FederationRoleSpoke, true, false)
	readMode := func(id int64, want string) {
		status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", id), nil)
		require.Equal(t, 200, status)
		require.Contains(t, string(raw), `"mode":"`+want+`"`)
	}
	readMode(project.ID, "none")
	_, err := env.DB.RenameProject(t.Context(), project.ID, "renamed-project")
	require.NoError(t, err)
	readMode(project.ID, "all")
	replacement := seedProject(t, env, "company-project")
	executionBinding(t, env.DB, replacement, db.FederationRoleSpoke, true, false)
	readMode(replacement.ID, "none")
	padded := seedProject(t, env, " spoke-project ")
	executionBinding(t, env.DB, padded, db.FederationRoleSpoke, true, false)
	readMode(padded.ID, "owned")
	unpadded := seedProject(t, env, "spoke-project")
	executionBinding(t, env.DB, unpadded, db.FederationRoleSpoke, true, false)
	readMode(unpadded.ID, "all")
}

type executionReadErrorStore struct {
	db.Storage
	fail string
}

func (s executionReadErrorStore) FederationBindingByProject(ctx context.Context, id int64) (db.FederationBinding, error) {
	if s.fail == "binding" {
		return db.FederationBinding{}, errors.New("lookup unavailable")
	}
	return s.Storage.FederationBindingByProject(ctx, id)
}
func (s executionReadErrorStore) CronRun(ctx context.Context, id int64, run string) (db.CronRun, error) {
	if s.fail == "run" {
		return db.CronRun{}, errors.New("lookup unavailable")
	}
	return s.Storage.CronRun(ctx, id, run)
}
func (s executionReadErrorStore) CronJob(ctx context.Context, id int64, job string) (db.CronJob, error) {
	if s.fail == "job" {
		return db.CronJob{}, errors.New("lookup unavailable")
	}
	return s.Storage.CronJob(ctx, id, job)
}
func (s executionReadErrorStore) CronWorkflow(ctx context.Context, id int64, workflow string) (db.CronWorkflow, error) {
	if s.fail == "workflow" {
		return db.CronWorkflow{}, errors.New("lookup unavailable")
	}
	return s.Storage.CronWorkflow(ctx, id, workflow)
}
func TestCronExecutionLookupErrors(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "company-project")
	executionBinding(t, env.DB, project, db.FederationRoleSpoke, true, false)
	wf, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "Inspect", Actor: "company-member", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "company-member", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	for _, fail := range []string{"binding", "run", "job", "workflow"} {
		t.Run(fail, func(t *testing.T) {
			store := executionReadErrorStore{Storage: env.DB, fail: fail}
			srv := daemon.NewServer(daemon.ServerConfig{DB: store, CronExecution: config.CronConfig{ReplicaExecute: "owned"}})
			t.Cleanup(func() { require.NoError(t, srv.Close()) })
			recorder := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t)), strings.NewReader(fmt.Sprintf(`{"actor":"observer","job_uid":%q,"definition_event_uid":%q,"workflow_uid":%q,"workflow_definition_event_uid":%q,"status":"running","summary":{"version":1},"expected_revision":0}`, job.UID, job.DefinitionEventUID, wf.UID, wf.DefinitionEventUID)))
			request.Header.Set("Content-Type", "application/json")
			srv.Handler().ServeHTTP(recorder, request)
			require.Equalf(t, 500, recorder.Code, "%s", recorder.Body.String())
		})
	}
}

func TestCronExecutionConfigWarnings(t *testing.T) {
	env := testenv.New(t)
	seedProject(t, env, "local-project")
	hub := seedProject(t, env, "hub-project")
	spoke := seedProject(t, env, "spoke-project")
	executionBinding(t, env.DB, hub, db.FederationRoleHub, true, false)
	executionBinding(t, env.DB, spoke, db.FederationRoleSpoke, true, false)
	warnings, err := daemon.CronExecutionWarnings(t.Context(), env.DB, config.CronConfig{Projects: []config.CronProjectConfig{{Name: "local-project", Execute: "none"}, {Name: "hub-project", Execute: "none"}, {Name: "spoke-project", Execute: "all"}, {Name: "future-project", Execute: "owned"}}})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		`cron.project "local-project" is not a replica; execution remains all`,
		`cron.project "hub-project" is not a replica; execution remains all`,
		`cron.project "future-project" matches no current project`,
	}, warnings)
}
