package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type cronTombstoneCase struct {
	kind, field string
	definition  any
}

func cronTombstoneCases() []cronTombstoneCase {
	return []cronTombstoneCase{
		{kind: "jobs", field: "job", definition: cronHTTPDefinition(nil)},
		{kind: "workflows", field: "workflow", definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}},
	}
}

// cronTombstoneDefinition creates a definition through the API and returns its
// item path and current state.
func cronTombstoneDefinition(t *testing.T, env *testenv.Env, tc cronTombstoneCase) (string, db.CronDefinition) {
	t.Helper()
	project := seedProject(t, env, "example-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/%s", project.ID, tc.kind)
	status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"actor": "worker", "name": "Inspect", "definition": tc.definition})
	require.Equalf(t, http.StatusCreated, status, "%s", raw)
	created := decodeCronTombstone(t, tc, raw)
	return path + "/" + created.UID, created
}

func decodeCronTombstone(t *testing.T, tc cronTombstoneCase, raw []byte) db.CronDefinition {
	t.Helper()
	var body struct {
		Job      *db.CronDefinition `json:"job"`
		Workflow *db.CronDefinition `json:"workflow"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	value := body.Job
	if tc.field == "workflow" {
		value = body.Workflow
	}
	require.NotNilf(t, value, "response carries %s: %s", tc.field, raw)
	return *value
}

// requireCronStateConflict asserts a lifecycle request to actionPath is
// refused as a cron conflict and leaves the definition at itemPath and the
// event log unchanged.
func requireCronStateConflict(t *testing.T, env *testenv.Env, tc cronTombstoneCase, method, actionPath, itemPath string, current db.CronDefinition) {
	t.Helper()
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	action := map[string]any{"actor": "worker", "expected_event_uid": current.DefinitionEventUID}
	status, raw := cronHTTP(t, env, method, actionPath, action)
	require.Equalf(t, http.StatusConflict, status, "%s", raw)
	require.Contains(t, string(raw), "cron_conflict")
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "a refused lifecycle change emits no event")
	status, raw = cronHTTP(t, env, http.MethodGet, itemPath, nil)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	require.Equal(t, current, decodeCronTombstone(t, tc, raw), "a refused lifecycle change leaves the definition unchanged")
}

// Restoring a live definition is not a lifecycle change, so it must not bump
// the revision or emit an update event.
func TestCronRestoreLiveDefinitionConflicts(t *testing.T) {
	for _, tc := range cronTombstoneCases() {
		t.Run(tc.field, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			path, live := cronTombstoneDefinition(t, env, tc)
			requireCronStateConflict(t, env, tc, http.MethodPost, path+"/restore", path, live)
		})
	}
}

// Deleting a deleted definition must not emit a second delete event or move
// its deletion time.
func TestCronDeleteDeletedDefinitionConflicts(t *testing.T) {
	for _, tc := range cronTombstoneCases() {
		t.Run(tc.field, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			path, live := cronTombstoneDefinition(t, env, tc)
			status, raw := cronHTTP(t, env, http.MethodDelete, path, map[string]any{"actor": "worker", "expected_event_uid": live.DefinitionEventUID})
			require.Equalf(t, http.StatusOK, status, "%s", raw)
			deleted := decodeCronTombstone(t, tc, raw)
			require.NotNil(t, deleted.DeletedAt)
			requireCronStateConflict(t, env, tc, http.MethodDelete, path, path, deleted)
		})
	}
}
