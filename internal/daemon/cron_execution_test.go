package daemon_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		root := cronHTTPUID(t)
		public, _, keyErr := ed25519.GenerateKey(nil)
		require.NoError(t, keyErr)
		require.NoError(t, store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		_, err = store.SetRelayBindingConfig(t.Context(), project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: cronHTTPUID(t), UpstreamInstanceUID: root, AuthorityUID: root, HubPath: []string{root, store.InstanceUID()}, LocalActor: "local-member", ServeDownstream: true, ResetEpoch: 1})
		require.NoError(t, err)
	}
}

func TestCronExecutionCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name           string
		role           db.FederationRole
		enabled, relay bool
		cfg            config.CronConfig
		want           api.CronExecutionPolicy
	}{
		{name: "local", want: api.CronExecutionPolicy{Mode: "all", Source: "local"}},
		{name: "hub", role: db.FederationRoleHub, want: api.CronExecutionPolicy{Mode: "all", Source: "local"}},
		{name: "direct", role: db.FederationRoleSpoke, enabled: true, want: api.CronExecutionPolicy{Replica: true, Mode: "none", MemberActor: "company-member", Source: "replica_default"}},
		{name: "disabled", role: db.FederationRoleSpoke, want: api.CronExecutionPolicy{Replica: true, Mode: "none", MemberActor: "company-member", Source: "replica_default"}},
		{name: "relay", role: db.FederationRoleSpoke, enabled: true, relay: true, cfg: config.CronConfig{ReplicaExecute: "owned"}, want: api.CronExecutionPolicy{Replica: true, Mode: "owned", MemberActor: "company-member", Source: "replica_default"}},
		{name: "override", role: db.FederationRoleSpoke, enabled: true, cfg: config.CronConfig{ReplicaExecute: "all", Projects: []config.CronProjectConfig{{Name: "company-project", Execute: "owned"}}}, want: api.CronExecutionPolicy{Replica: true, Mode: "owned", MemberActor: "company-member", Source: "project"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CronExecution = tc.cfg })
			project := seedProject(t, env, "company-project")
			if tc.role != "" {
				executionBinding(t, env.DB, project, tc.role, tc.enabled, tc.relay)
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
		name, mode, author, kind string
		workflowAuthor           string
		actionWorkflow           bool
		workflow, deleted, relay bool
		want                     int
	}{
		{name: "none", author: "company-member", want: 403},
		{name: "owned author", mode: "owned", author: "company-member", want: 200},
		{name: "owned other", mode: "owned", author: "other-member", want: 403},
		{name: "relay upstream", mode: "owned", author: "company-member", relay: true, want: 200},
		{name: "relay local", mode: "owned", author: "local-member", relay: true, want: 403},
		{name: "all", mode: "all", author: "other-member", want: 200},
		{name: "workflow owned", mode: "owned", author: "company-member", workflow: true, want: 200},
		{name: "workflow other", mode: "owned", author: "other-member", workflow: true, want: 403},
		{name: "workflow none", author: "company-member", workflow: true, want: 403},
		{name: "notify", author: "other-member", kind: "notify", want: 200},
		{name: "notify workflow", author: "other-member", kind: "notify", workflow: true, want: 403},
		{name: "execute with foreign body workflow", mode: "owned", author: "company-member", kind: "execute", workflow: true, workflowAuthor: "other-member", want: 403},
		{name: "execute with owned body workflow", mode: "owned", author: "company-member", kind: "execute", workflow: true, workflowAuthor: "company-member", want: 200},
		{name: "owned job foreign action workflow", mode: "owned", author: "company-member", actionWorkflow: true, workflowAuthor: "other-member", want: 403},
		{name: "owned job owned action workflow", mode: "owned", author: "company-member", actionWorkflow: true, workflowAuthor: "company-member", want: 200},
		{name: "tombstone", mode: "owned", author: "company-member", deleted: true, want: 200},
		{name: "tombstone other", mode: "owned", author: "other-member", deleted: true, want: 403},
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
				if tc.deleted {
					job, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Actor: tc.author, Definition: def, Deleted: true})
					require.NoError(t, err)
				}
				body["job_uid"], body["definition_event_uid"] = job.UID, job.DefinitionEventUID
			}
			executionBinding(t, env.DB, project, db.FederationRoleSpoke, true, tc.relay)
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			status, raw := cronHTTP(t, env, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t)), body)
			require.Equalf(t, tc.want, status, "%s", raw)
			if tc.want == 403 {
				var problem api.ErrorEnvelope
				require.NoError(t, json.Unmarshal(raw, &problem))
				require.Equal(t, "cron_execution_not_enabled", problem.Error.Code)
				require.Contains(t, problem.Error.Hint, "[[cron.project]]")
				require.Contains(t, problem.Error.Message, "replicated project")
				mode := tc.mode
				if mode == "" {
					mode = "none"
				}
				require.Equal(t, mode, problem.Error.Data["mode"])
				require.Equal(t, "company-member", problem.Error.Data["member_actor"])
				after, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
		})
	}
}

func TestCronExecutionExistingRunAndMissingDefinition(t *testing.T) {
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
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "replay: %s", raw)
	body["status"], body["expected_revision"] = "succeeded", 1
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "finish: %s", raw)
	body["job_uid"] = cronHTTPUID(t)
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 409, status, "identity: %s", raw)
	body["job_uid"] = job.UID
	// Reproduce retained run history without a materialized definition, as can
	// happen while federation catches up. This is fixture-local state only.
	_, err = env.DB.ExecContext(t.Context(), "DELETE FROM cron_jobs WHERE uid=?", job.UID)
	require.NoError(t, err)
	status, raw = cronHTTP(t, env, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t)), body)
	require.Equalf(t, 403, status, "new run must not borrow retained evidence: %s", raw)
}

func TestCronExecutionOverrideFollowsCurrentName(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.CronExecution = config.CronConfig{ReplicaExecute: "all", Projects: []config.CronProjectConfig{{Name: "company-project", Execute: "none"}}}
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
}

func TestCronExecutionOverrideMatchesExactStoredProjectName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	configFile := "[cron]\nreplica_execute = 'all'\n[[cron.project]]\nname = ' spoke-project '\nexecute = 'none'\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(configFile), 0600))
	daemonConfig, err := config.ReadDaemonConfig()
	require.NoError(t, err)

	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CronExecution = daemonConfig.Cron })
	project := seedProject(t, env, " spoke-project ")
	executionBinding(t, env.DB, project, db.FederationRoleSpoke, true, false)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "company-member", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)

	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", project.ID), nil)
	require.Equalf(t, 200, status, "%s", raw)
	require.Contains(t, string(raw), `"mode":"none"`)
	body := map[string]any{"actor": "observer", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "running", "summary": map[string]any{"version": 1}, "expected_revision": 0}
	status, raw = cronHTTP(t, env, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t)), body)
	require.Equalf(t, 403, status, "padded exact project-name override: %s", raw)
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
func TestCronExecutionLookupErrors(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "company-project")
	executionBinding(t, env.DB, project, db.FederationRoleSpoke, true, false)
	for _, fail := range []string{"binding", "run", "job"} {
		t.Run(fail, func(t *testing.T) {
			srv := daemon.NewServer(daemon.ServerConfig{DB: executionReadErrorStore{Storage: env.DB, fail: fail}})
			t.Cleanup(func() { require.NoError(t, srv.Close()) })
			recorder := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t)), strings.NewReader(fmt.Sprintf(`{"actor":"observer","job_uid":%q,"definition_event_uid":%q,"status":"running","summary":{"version":1},"expected_revision":0}`, cronHTTPUID(t), cronHTTPUID(t))))
			request.Header.Set("Content-Type", "application/json")
			srv.Handler().ServeHTTP(recorder, request)
			require.Equalf(t, 500, recorder.Code, "%s", recorder.Body.String())
		})
	}
}

func TestCronExecutionConfigWarnings(t *testing.T) {
	env := testenv.New(t)
	local := seedProject(t, env, "local-project")
	hub := seedProject(t, env, "hub-project")
	spoke := seedProject(t, env, "spoke-project")
	_ = local
	executionBinding(t, env.DB, hub, db.FederationRoleHub, true, false)
	executionBinding(t, env.DB, spoke, db.FederationRoleSpoke, true, false)
	warnings, err := daemon.CronExecutionWarnings(t.Context(), env.DB, config.CronConfig{Projects: []config.CronProjectConfig{{Name: "local-project", Execute: "none"}, {Name: "hub-project", Execute: "none"}, {Name: "spoke-project", Execute: "all"}, {Name: "future-project", Execute: "owned"}}})
	require.NoError(t, err)
	require.Len(t, warnings, 3)
	require.Contains(t, strings.Join(warnings, "\n"), "future-project")
}

func TestCronExecutionNonCurrentNotifyDefinition(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "spoke-project")
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Inspect", Author: "member-a"})
	require.NoError(t, err)
	def := cronHTTPDefinition(t)
	def.Action = cron.Action{Kind: "notify", Recipient: "member-a", Message: "Inspect"}
	def.Issue = &cron.IssuePolicy{Kind: "existing", UID: issue.UID}
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "member-a", Definition: def})
	require.NoError(t, err)
	oldEvent := job.DefinitionEventUID
	def.Action.Message = "Inspect again"
	job, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: oldEvent, Name: job.Name, Actor: "member-a", Definition: def})
	require.NoError(t, err)
	executionBinding(t, env.DB, project, db.FederationRoleSpoke, true, false)
	body := map[string]any{"actor": "observer", "job_uid": job.UID, "definition_event_uid": oldEvent, "status": "running", "summary": map[string]any{"version": 1}, "expected_revision": 0}
	status, raw := cronHTTP(t, env, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t)), body)
	require.Equalf(t, http.StatusForbidden, status, "%s", raw)
	var problem api.ErrorEnvelope
	require.NoError(t, json.Unmarshal(raw, &problem))
	require.Equal(t, "cron_execution_not_enabled", problem.Error.Code)
}
