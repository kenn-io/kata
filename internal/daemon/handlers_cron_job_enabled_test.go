package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func decodeCronJobResponse(t *testing.T, raw []byte) api.CronJobResponse {
	t.Helper()
	var out api.CronJobResponse
	require.NoError(t, json.Unmarshal(raw, &out.Body), string(raw))
	return out
}

// Contract: actions change enabled alone, use CAS even for a no-op, and never
// restore a tombstone. A repeated action at the root emits no event.
func TestCronJobPauseResume(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s", project.ID, job.UID)
	original := job.DefinitionEventUID
	for _, action := range []string{"pause", "resume"} {
		status, raw := cronHTTP(t, env, http.MethodPost, path+"/"+action, map[string]any{"actor": "operator"})
		require.Equalf(t, 200, status, "%s", raw)
		out := decodeCronJobResponse(t, raw)
		require.Len(t, out.Body.Events, 1)
		require.Equal(t, "cron.job.updated", out.Body.Events[0].Type)
		require.Equal(t, "operator", out.Body.Events[0].Actor)
		current := out.Body.Job.DefinitionEventUID
		before, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		status, raw = cronHTTP(t, env, http.MethodPost, path+"/"+action, map[string]any{"actor": "operator", "expected_event_uid": current})
		require.Equalf(t, 200, status, "%s", raw)
		out = decodeCronJobResponse(t, raw)
		require.Empty(t, out.Body.Events)
		require.Equal(t, current, out.Body.Job.DefinitionEventUID)
		after, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, after)
		status, raw = cronHTTP(t, env, http.MethodPost, path+"/"+action, map[string]any{"actor": "operator", "expected_event_uid": original})
		require.Equalf(t, 409, status, "%s", raw)
		require.Contains(t, string(raw), `"code":"cron_conflict"`)
		require.Contains(t, string(raw), current)
		original = current
	}
	stored, err := env.DB.CronJob(t.Context(), project.ID, job.UID)
	require.NoError(t, err)
	_, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: stored.DefinitionEventUID, Name: job.Name, Actor: "operator", Definition: stored.Definition, Deleted: true})
	require.NoError(t, err)
	for _, action := range []string{"pause", "resume"} {
		status, raw := cronHTTP(t, env, http.MethodPost, path+"/"+action, map[string]any{"actor": "operator"})
		require.Equalf(t, 409, status, "%s", raw)
	}
}

// Contract: the actions rewrite only enabled, preserving every other stored
// definition field, including exact opaque option numbers.
func TestCronJobEnabledPreservesDefinition(t *testing.T) {
	env, project, _, native := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
	offset := int64(-86400)
	adapter := cron.JobDefinition{
		Version: 1, Kind: "job", Enabled: true,
		Trigger: cron.Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: "UTC"},
		Action:  cron.Action{Kind: "execute", Prompt: "Inspect"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Review", Body: "Escaped\ntext", ScheduledOffsetSeconds: &offset, DeadlineOffsetSeconds: &offset},
		Overlap: "forbid", Catchup: "latest", GraceSeconds: 120, TimeoutSeconds: 900,
		CheckoutKey: "checkout-1", SecretRefs: map[string]string{"access": "example-secret"},
		Options: jsontext.Value(`{"opaque":9007199254740993,"nested":[null,true,{"n":1.234567890123456789}]}`),
	}
	for _, definition := range []cron.JobDefinition{native, adapter} {
		job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: definition})
		require.NoError(t, err)
		for _, action := range []string{"pause", "resume"} {
			status, raw := cronHTTP(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/%s", project.ID, job.UID, action), map[string]any{"actor": "operator"})
			require.Equalf(t, 200, status, "%s", raw)
			want := job.Definition
			want.Enabled = action == "resume"
			require.Equal(t, want, decodeCronJobResponse(t, raw).Body.Job.Definition.Native())
			stored, err := env.DB.CronJob(t.Context(), project.ID, job.UID)
			require.NoError(t, err)
			require.Equal(t, want, stored.Definition)
		}
	}
}

// Contract: an unguarded action is still guarded by the revision it read, so a
// concurrent edit between that read and the write wins with a conflict.
func TestCronJobPauseLosesConcurrentEdit(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	definition := cronHTTPDefinition(t)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	store := beforeCronJobStore{Storage: env.DB, before: func() error {
		changed := definition
		changed.Action.Prompt = "Concurrent edit"
		_, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: "Edited", Actor: "operator", Definition: changed})
		return err
	}}
	server := daemon.NewServer(daemon.ServerConfig{DB: store})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	target := &testenv.Env{URL: endpoint.URL, HTTP: endpoint.Client()}
	response, raw := envDoRaw(t, target, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/pause", project.ID, job.UID), map[string]any{"actor": "operator"}, nil)
	require.Equalf(t, 409, response.StatusCode, "%s", raw)
	current, err := env.DB.CronJob(t.Context(), project.ID, job.UID)
	require.NoError(t, err)
	require.True(t, current.Definition.Enabled)
	require.Equal(t, "Concurrent edit", current.Definition.Action.Prompt)
}

// Contract: credentials with a bound actor may omit the optional action body
// and may discard a native job's backlog under a retained revision.
func TestCronJobEnabledEnrollment(t *testing.T) {
	env, project, _, definition := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "tester", Definition: definition})
	require.NoError(t, err)
	enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "push")
	headers := map[string]string{"Authorization": "Bearer " + enrollment.Token}
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/", project.ID, job.UID)
	var paused api.CronJobResponse
	for _, action := range []string{"resume", "pause"} {
		response, raw := envDoRaw(t, env, http.MethodPost, path+action, nil, headers)
		require.Equalf(t, 200, response.StatusCode, "%s", raw)
		paused = decodeCronJobResponse(t, raw)
	}
	response, raw := envDoRaw(t, env, http.MethodPost, path+"resume", map[string]any{"discard_missed": true, "expected_event_uid": paused.Body.Job.DefinitionEventUID}, headers)
	require.Equalf(t, 200, response.StatusCode, "%s", raw)
	require.True(t, decodeCronJobResponse(t, raw).Body.Job.Definition.Enabled)
}
