package daemon_test

import (
	"context"
	"encoding/json/jsontext"
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
		name, mode, author, kind string
		workflowAuthor, editor   string
		stale, missingJob        bool
		actionWorkflow           bool
		workflow, deleted, relay bool
		want                     int
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
		{name: "typo job", author: "company-member", missingJob: true, want: 400},
		{name: "job other edit", mode: "owned", author: "company-member", editor: "other-member", want: 403},
		{name: "action workflow other edit", mode: "owned", author: "company-member", actionWorkflow: true, editor: "other-member", want: 403},
		{name: "observed workflow other edit", mode: "owned", author: "company-member", workflow: true, editor: "other-member", want: 403},
		{name: "notify", author: "other-member", kind: "notify", want: 200},
		{name: "notify workflow", author: "other-member", kind: "notify", workflow: true, want: 403},
		{name: "execute with foreign body workflow", mode: "owned", author: "company-member", kind: "execute", workflow: true, workflowAuthor: "other-member", want: 403},
		{name: "execute with owned body workflow", mode: "owned", author: "company-member", kind: "execute", workflow: true, workflowAuthor: "company-member", want: 200},
		{name: "owned job foreign action workflow", mode: "owned", author: "company-member", actionWorkflow: true, workflowAuthor: "other-member", want: 403},
		{name: "owned job owned action workflow", mode: "owned", author: "company-member", actionWorkflow: true, workflowAuthor: "company-member", want: 200},
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
				if tc.missingJob {
					body["job_uid"], body["definition_event_uid"] = cronHTTPUID(t), cronHTTPUID(t)
				}
			}
			executionBinding(t, env.DB, project, db.FederationRoleSpoke, true, tc.relay)
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			status, raw := cronHTTP(t, env, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t)), body)
			require.Equalf(t, tc.want, status, "%s", raw)
			if tc.want == 400 {
				var problem api.ErrorEnvelope
				require.NoError(t, json.Unmarshal(raw, &problem))
				require.Equal(t, "validation", problem.Error.Code)
				require.Contains(t, problem.Error.Message, "definition event reference")
			}
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
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "replay: %s", raw)
	body["status"], body["expected_revision"] = "succeeded", 1
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "finish: %s", raw)
	body["job_uid"] = cronHTTPUID(t)
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, 409, status, "identity: %s", raw)
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
	for _, fail := range []string{"binding", "run", "job", "workflow", "event"} {
		t.Run(fail, func(t *testing.T) {
			var store db.Storage = executionReadErrorStore{Storage: env.DB, fail: fail}
			if fail == "event" {
				store = ingestEventReadFailureStore{env.DB}
			}
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

func TestCronExecutionOwnedBaselineThenEdit(t *testing.T) {
	for _, workflow := range []bool{false, true} {
		t.Run(fmt.Sprintf("workflow=%t", workflow), func(t *testing.T) {
			hub := testenv.New(t)
			project := createFederatedHubProject(t, hub, "hub-project")
			var definition db.CronDefinition
			var original db.Event
			if workflow {
				wf, event, err := hub.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "Inspect", Actor: "company-member", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}})
				require.NoError(t, err)
				definition, original = wf.CronDefinition, event
			} else {
				job, events, err := hub.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "company-member", Definition: cronHTTPDefinition(t)})
				require.NoError(t, err)
				definition, original = job.CronDefinition, events[0]
			}
			tx, err := hub.DB.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			snapshots, err := db.CronDefinitionSnapshots(t.Context(), tx, project)
			require.NoError(t, err)
			require.NoError(t, tx.Rollback())
			require.Len(t, snapshots, 1)
			spoke := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CronExecution.ReplicaExecute = "owned" })
			replica, err := spoke.DB.CreateProjectWithUID(t.Context(), "spoke-project", project.UID)
			require.NoError(t, err)
			executionBinding(t, spoke.DB, replica, db.FederationRoleSpoke, true, false)
			snapshot := db.RemoteEvent{
				EventUID: cronHTTPUID(t), OriginInstanceUID: original.OriginInstanceUID,
				ProjectUID: project.UID, ProjectName: project.Name, Type: snapshots[0].Type,
				Actor: "baseline-writer", HLCPhysicalMS: original.HLCPhysicalMS,
				HLCCounter: original.HLCCounter, Payload: jsontext.Value(snapshots[0].Payload), CreatedAt: original.CreatedAt,
			}
			snapshot.ContentHash, err = db.RemoteEventContentHash(snapshot)
			require.NoError(t, err)
			inserted, err := spoke.DB.InsertRemoteEvent(t.Context(), replica.ID, snapshot)
			require.NoError(t, err)
			require.True(t, inserted)
			require.NoError(t, spoke.DB.MaterializeFederatedProject(t.Context(), replica.ID))
			events, err := spoke.DB.EventsByUIDs(t.Context(), replica.ID, []string{definition.DefinitionEventUID})
			require.ErrorIs(t, err, db.ErrNotFound)
			require.Empty(t, events)
			body := map[string]any{"actor": "observer", "status": "running", "summary": map[string]any{"version": 1}, "expected_revision": 0}
			uidKey, eventKey := "job_uid", "definition_event_uid"
			if workflow {
				uidKey, eventKey = "workflow_uid", "workflow_definition_event_uid"
			}
			body[uidKey], body[eventKey] = definition.UID, definition.DefinitionEventUID
			path := fmt.Sprintf("/api/v1/projects/%d/cron/runs/", replica.ID)
			status, raw := cronHTTP(t, spoke, http.MethodPut, path+cronHTTPUID(t), body)
			require.Equalf(t, 200, status, "baseline: %s", raw)

			var edited db.Event
			if workflow {
				wf, event, err := hub.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, UID: definition.UID, ExpectedEventUID: definition.DefinitionEventUID, Name: "Updated", Actor: "other-member", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "echo changed"}}}})
				require.NoError(t, err)
				definition, edited = wf.CronDefinition, event
			} else {
				job, events, err := hub.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: definition.UID, ExpectedEventUID: definition.DefinitionEventUID, Name: "Updated", Actor: "other-member", Definition: cronHTTPDefinition(t)})
				require.NoError(t, err)
				definition, edited = job.CronDefinition, events[0]
			}
			inserted, err = spoke.DB.InsertRemoteEvent(t.Context(), replica.ID, db.RemoteEvent{
				EventUID: edited.UID, OriginInstanceUID: edited.OriginInstanceUID,
				ProjectUID: edited.ProjectUID, ProjectName: edited.ProjectName, Type: edited.Type,
				Actor: edited.Actor, HLCPhysicalMS: edited.HLCPhysicalMS, HLCCounter: edited.HLCCounter,
				ContentHash: edited.ContentHash, Payload: jsontext.Value(edited.Payload), CreatedAt: edited.CreatedAt,
			})
			require.NoError(t, err)
			require.True(t, inserted)
			require.NoError(t, spoke.DB.MaterializeFederatedProject(t.Context(), replica.ID))
			body[eventKey] = definition.DefinitionEventUID
			status, raw = cronHTTP(t, spoke, http.MethodPut, path+cronHTTPUID(t), body)
			require.Equalf(t, 403, status, "colleague edit: %s", raw)
			var problem api.ErrorEnvelope
			require.NoError(t, json.Unmarshal(raw, &problem))
			require.Equal(t, "cron_execution_not_enabled", problem.Error.Code)
		})
	}
}
