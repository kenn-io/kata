package daemon_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// Reads use the current (even tombstoned) job policy, without modifying the
// run's historical references, timestamps, revision, or event history.
func TestCronRunStalenessReads(t *testing.T) {
	var now time.Time
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) {
		cfg.CronClock = func() time.Time { return now }
	})
	project := staleProject(t, env, "example-project")
	definition := staleDefinition(t)
	definition.TimeoutSeconds = 3600
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Example job", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	input := db.ObserveCronRun{ProjectID: project.ID, UID: staleUID(t), Actor: "worker", JobUID: new(job.UID), DefinitionEventUID: new(job.DefinitionEventUID), Status: "running", Summary: cron.Summary{Version: 1}}
	result, err := env.DB.ObserveCronRun(t.Context(), input)
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runs", project.ID)
	read := func(stale bool, delay time.Duration, basis string) {
		t.Helper()
		beforeRead, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		for _, route := range []string{path + "/" + input.UID, path} {
			status, raw := staleHTTP(t, env, http.MethodGet, route, nil)
			require.Equalf(t, 200, status, "%s", raw)
			var response struct {
				Run  map[string]any   `json:"run"`
				Runs []map[string]any `json:"runs"`
			}
			require.NoError(t, json.Unmarshal(raw, &response))
			run := response.Run
			if route == path {
				require.Len(t, response.Runs, 1)
				run = response.Runs[0]
			}
			require.Equal(t, map[string]any{"stale": stale, "basis": basis, "stale_at": result.Run.UpdatedAt.Add(delay).Format(time.RFC3339Nano)}, run["staleness"])
			delete(run, "staleness")
			want, err := json.Marshal(result.Run)
			require.NoError(t, err)
			got, err := json.Marshal(run)
			require.NoError(t, err)
			require.JSONEq(t, string(want), string(got))
		}
		afterRead, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, beforeRead, afterRead, "read-time age emits no events")
	}
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	now = result.Run.UpdatedAt.Add(75 * time.Minute).Add(-time.Nanosecond)
	read(false, 75*time.Minute, "timeout_seconds")
	now = now.Add(time.Nanosecond)
	read(true, 75*time.Minute, "timeout_seconds")
	definition.TimeoutSeconds = 7200
	job, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Actor: "worker", Definition: definition, Deleted: true})
	require.NoError(t, err)
	read(false, 135*time.Minute, "timeout_seconds")
	afterJob, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Greater(t, afterJob, before)
	input.Status, input.ExpectedRevision = "succeeded", result.Run.Revision
	_, err = env.DB.ObserveCronRun(t.Context(), input)
	require.NoError(t, err)
	status, raw := staleHTTP(t, env, http.MethodGet, path+"/"+input.UID, nil)
	require.Equalf(t, 200, status, "%s", raw)
	require.NotContains(t, string(raw), `"staleness"`)
	// All reads above left the original evidence unchanged.
	stored, err := env.DB.CronRun(t.Context(), project.ID, input.UID)
	require.NoError(t, err)
	require.Equal(t, result.Run.Revision+1, stored.Revision)
}

type stalenessJobFaultStore struct {
	db.Storage
	fault *error
}

func (s stalenessJobFaultStore) CronJob(ctx context.Context, project int64, id string) (db.CronJob, error) {
	if *s.fault != nil {
		return db.CronJob{}, *s.fault
	}
	return s.Storage.CronJob(ctx, project, id)
}
func (s stalenessJobFaultStore) ListCronJobs(ctx context.Context, in db.CronList) ([]db.CronJob, error) {
	if *s.fault != nil {
		return nil, *s.fault
	}
	return s.Storage.ListCronJobs(ctx, in)
}

func TestCronRunStalenessLookupFailures(t *testing.T) {
	var fault error
	var now time.Time
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) {
		cfg.DB = stalenessJobFaultStore{cfg.DB, &fault}
		cfg.CronClock = func() time.Time { return now }
	})
	project := staleProject(t, env, "example-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Example job", Actor: "worker", Definition: staleDefinition(t)})
	require.NoError(t, err)
	result, err := env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: staleUID(t), Actor: "worker", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	now = result.Run.UpdatedAt.Add(24 * time.Hour)
	base := fmt.Sprintf("/api/v1/projects/%d/cron/runs", project.ID)
	fault = db.ErrNotFound
	status, raw := staleHTTP(t, env, http.MethodGet, base+"/"+result.Run.UID, nil)
	require.Equalf(t, 200, status, "%s", raw)
	require.Contains(t, string(raw), `"basis":"default"`)
	for _, failure := range []error{errors.New("job decode failed"), context.Canceled} {
		fault = failure
		for _, route := range []string{base, base + "/" + result.Run.UID} {
			status, raw = staleHTTP(t, env, http.MethodGet, route, nil)
			require.Equalf(t, 500, status, "%s", raw)
		}
	}
}

func TestCronWorkflowRunStalenessDefault(t *testing.T) {
	var now time.Time
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) { cfg.CronClock = func() time.Time { return now } })
	project := staleProject(t, env, "example-project")
	w, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "Example flow", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	r, err := env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: staleUID(t), Actor: "worker", WorkflowUID: &w.UID, WorkflowDefinitionEventUID: &w.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	now = r.Run.UpdatedAt.Add(24 * time.Hour)
	for _, suffix := range []string{"", "/" + r.Run.UID} {
		status, raw := staleHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs%s", project.ID, suffix), nil)
		require.Equalf(t, 200, status, "%s", raw)
		require.Contains(t, string(raw), `"basis":"default"`)
		require.Contains(t, string(raw), `"stale":true`)
	}
}

// These small fixture helpers keep this handler contract runnable as one file
// on shared hosts without compiling every unrelated daemon integration test.
func staleProject(t *testing.T, env *testenv.Env, name string) db.Project {
	t.Helper()
	p, err := env.DB.CreateProject(t.Context(), name)
	require.NoError(t, err)
	return p
}
func staleDefinition(_ *testing.T) cron.JobDefinition {
	return cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Inspection"}, Overlap: "forbid", Catchup: "skip"}
}
func staleUID(t *testing.T) string {
	t.Helper()
	v, err := uid.New()
	require.NoError(t, err)
	return v
}
func staleHTTP(t *testing.T, env *testenv.Env, method, path string, body any) (int, []byte) {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, env.URL+path, strings.NewReader(string(raw)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	res, err := env.HTTP.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, res.Body.Close()) }()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, data
}
