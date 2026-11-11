package daemon_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestWorkflowConflictRespectsReaderFeatures(t *testing.T) {
	for _, features := range []string{"", "cron_workflow_argv_v1_extra", db.CronWorkflowArgvEventFeature} {
		t.Run(features, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			project := seedProject(t, env, "example-project")
			command, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "command", Actor: "first", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "run", Kind: "command", Command: "true"}}}})
			require.NoError(t, err)
			current, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, UID: command.UID, ExpectedEventUID: command.DefinitionEventUID, Name: "argv", Actor: "second", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "run", Kind: "argv", Argv: []string{"printf", ""}}}}})
			require.NoError(t, err)
			path := fmt.Sprintf("/api/v1/projects/%d/cron/workflows/%s", project.ID, command.UID)
			body := fmt.Sprintf(`{"actor":"first","name":"command","expected_event_uid":%q,"definition":{"version":1,"steps":[{"key":"run","kind":"command","command":"true"}]}}`, command.DefinitionEventUID)
			requests := []struct{ method, path, body string }{
				{http.MethodPut, path, body},
				{http.MethodDelete, path, fmt.Sprintf(`{"actor":"first","expected_event_uid":%q}`, command.DefinitionEventUID)},
				{http.MethodPost, path + "/restore", fmt.Sprintf(`{"actor":"first","expected_event_uid":%q}`, command.DefinitionEventUID)},
			}
			for _, request := range requests {
				status, raw := argvRequest(t, env, request.method, request.path, request.body, features)
				require.Equalf(t, http.StatusConflict, status, "%s", raw)
				var response api.ErrorEnvelope
				require.NoError(t, json.Unmarshal([]byte(raw), &response))
				require.Equal(t, "cron_conflict", response.Error.Code)
				require.NotEmpty(t, response.Error.Hint)
				if features == db.CronWorkflowArgvEventFeature {
					require.Contains(t, response.Error.Data, "current")
					require.Contains(t, raw, current.DefinitionEventUID)
				} else {
					require.NotContains(t, response.Error.Data, "current")
					require.NotContains(t, raw, "printf")
				}
			}
		})
	}
}

func TestWorkflowLifecycleRefusesUnreadableDefinitionBeforeMutation(t *testing.T) {
	for _, restore := range []bool{false, true} {
		for _, features := range []string{"", "cron_workflow_argv_v1_extra"} {
			t.Run(fmt.Sprintf("restore=%t/features=%s", restore, features), func(t *testing.T) {
				env := testenv.New(t, testenv.WithAuthToken("tok"))
				project := seedProject(t, env, "example-project")
				workflow, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "argv", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "run", Kind: "argv", Argv: []string{"printf", ""}}}}})
				require.NoError(t, err)
				if restore {
					workflow, _, err = env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, UID: workflow.UID, ExpectedEventUID: workflow.DefinitionEventUID, Name: workflow.Name, Actor: "worker", Definition: workflow.Definition, Deleted: true})
					require.NoError(t, err)
				}
				before, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				path := fmt.Sprintf("/api/v1/projects/%d/cron/workflows/%s", project.ID, workflow.UID)
				method := http.MethodDelete
				if restore {
					method, path = http.MethodPost, path+"/restore"
				}
				body := fmt.Sprintf(`{"actor":"worker","expected_event_uid":%q}`, workflow.DefinitionEventUID)
				status, raw := argvRequest(t, env, method, path, body, features)
				require.Equalf(t, http.StatusConflict, status, "%s", raw)
				require.Contains(t, raw, "unsupported_event_features")
				after, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after)
				got, err := env.DB.CronWorkflow(t.Context(), project.ID, workflow.UID)
				require.NoError(t, err)
				require.Equal(t, workflow, got)
			})
		}
	}
}

// Replace the readable snapshot at the write boundary, so the conflict handler
// must check the actual reread rather than the earlier lifecycle snapshot.
type workflowConflictRaceStore struct {
	db.Storage
	once      sync.Once
	injectErr error
}

func (s *workflowConflictRaceStore) PutCronWorkflow(ctx context.Context, p db.PutCronWorkflow) (db.CronWorkflow, db.Event, error) {
	s.once.Do(func() {
		_, _, s.injectErr = s.Storage.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: p.ProjectID, UID: p.UID, ExpectedEventUID: p.ExpectedEventUID, Name: "concurrent", Actor: "second", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "run", Kind: "argv", Argv: []string{"printf", ""}}}}})
	})
	return s.Storage.PutCronWorkflow(ctx, p)
}
func TestWorkflowLifecycleConflictChecksRereadFeatures(t *testing.T) {
	var race *workflowConflictRaceStore
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) { race = &workflowConflictRaceStore{Storage: cfg.DB}; cfg.DB = race })
	project := seedProject(t, env, "example-project")
	workflow, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "command", Actor: "first", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "run", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/cron/workflows/%s", project.ID, workflow.UID)
	status, raw := argvRequest(t, env, http.MethodDelete, path, fmt.Sprintf(`{"actor":"first","expected_event_uid":%q}`, workflow.DefinitionEventUID), "")
	require.NoError(t, race.injectErr)
	require.Equalf(t, http.StatusConflict, status, "%s", raw)
	var response api.ErrorEnvelope
	require.NoError(t, json.Unmarshal([]byte(raw), &response))
	require.Equal(t, "cron_conflict", response.Error.Code)
	require.NotContains(t, response.Error.Data, "current")
	require.NotContains(t, raw, "printf")
}

func TestWorkflowRawValidationFollowsBootstrapAttributionRefusal(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"), testenv.WithRequireTokenIdentity())
	project := seedProject(t, env, "example-project")
	workflow, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "command", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "run", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/cron/workflows", project.ID)
	for _, tc := range []struct{ method, path, expected string }{{http.MethodPost, path, ""}, {http.MethodPut, path + "/" + workflow.UID, workflow.DefinitionEventUID}} {
		body := fmt.Sprintf(`{"actor":"worker","name":"command","expected_event_uid":%q,"definition":{"version":1,"steps":[{"key":"run","kind":"command","command":"true","argv":null}]}}`, tc.expected)
		status, raw := argvRequest(t, env, tc.method, tc.path, body, "")
		require.Equalf(t, http.StatusForbidden, status, "%s", raw)
	}
}
