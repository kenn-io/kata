package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"hegel.dev/go/hegel"
)

// Fixtures are ordinary cron rows in the isolated test database. Direct inserts
// control creation order without changing the production clock or schema; each
// run references its own job's project and definition event, as producers require.
func seedCoreVisibilityRun(t *testing.T, env *testenv.Env, n int, job db.CronJob, status string, at time.Time) string {
	t.Helper()
	id := fmt.Sprintf("01J%023d", n)
	_, err := env.DB.ExecContext(t.Context(), `INSERT INTO cron_runs
		(uid,project_id,job_uid,definition_event_uid,actor,status,created_at,updated_at)
		VALUES($1,$2,$3,$4,'worker',$5,$6,$6)`, id, job.ProjectID, job.UID, job.DefinitionEventUID, status, at.UTC().Format(db.CronRunCreatedAtFormat))
	require.NoError(t, err)
	return id
}

func coreVisibilityJob(t *testing.T, env *testenv.Env, project int64, name string) db.CronJob {
	t.Helper()
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project, Name: name, Definition: cronHTTPDefinition(t), Actor: "worker"})
	require.NoError(t, err)
	return job
}

// The catalog must find a job's latest run outside the loaded history page,
// order timestamp ties by UID, and keep another project's evidence out.
func TestCronCoreLatestAcrossHistory(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	other := seedProject(t, env, "other-project")
	quiet := coreVisibilityJob(t, env, project.ID, "Quiet")
	busy := coreVisibilityJob(t, env, project.ID, "Busy")
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	seedCoreVisibilityRun(t, env, 1, quiet, "failed", at)
	winner := seedCoreVisibilityRun(t, env, 2, quiet, "succeeded", at)
	for i := range 105 {
		seedCoreVisibilityRun(t, env, 10+i, busy, "running", at.Add(time.Hour))
	}
	foreignJob := coreVisibilityJob(t, env, other.ID, "Quiet")
	seedCoreVisibilityRun(t, env, 200, foreignJob, "failed", at.Add(24*time.Hour))
	base := fmt.Sprintf("/api/v1/projects/%d/cron", project.ID)
	for _, path := range []string{base + "/jobs", base + "/jobs/" + quiet.UID} {
		status, raw := cronHTTP(t, env, http.MethodGet, path, nil)
		require.Equalf(t, 200, status, "%s", raw)
		var response struct {
			Job struct {
				LastRun *db.CronRun `json:"last_run"`
			} `json:"job"`
			Jobs []struct {
				UID     string      `json:"uid"`
				LastRun *db.CronRun `json:"last_run"`
			} `json:"jobs"`
		}
		require.NoError(t, json.Unmarshal(raw, &response))
		last := response.Job.LastRun
		for _, job := range response.Jobs {
			if job.UID == quiet.UID {
				last = job.LastRun
			}
		}
		require.NotNil(t, last, "latest run is authoritative beyond the history page")
		require.Equal(t, winner, last.UID)
	}
}

func TestCronCoreNeverRanFilter(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	never := coreVisibilityJob(t, env, project.ID, "Never")
	ran := coreVisibilityJob(t, env, project.ID, "Ran")
	seedCoreVisibilityRun(t, env, 1, ran, "succeeded", time.Now().UTC())
	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs?status=never_ran", project.ID), nil)
	require.Equalf(t, 200, status, "%s", raw)
	var response struct {
		Jobs []map[string]jsontext.Value `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Len(t, response.Jobs, 1, "never_ran must select only jobs with no history")
	var id string
	require.NoError(t, json.Unmarshal(response.Jobs[0]["uid"], &id))
	require.Equal(t, never.UID, id)
	require.Equal(t, "null", string(response.Jobs[0]["last_run"]))
}

// Repeated status parameters form a union, applied before the page limit;
// a cursor can itself be outside the selected statuses.
func TestCronCoreStatusPages(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	other := seedProject(t, env, "other-project")
	job := coreVisibilityJob(t, env, project.ID, "Inspect")
	otherJob := coreVisibilityJob(t, env, project.ID, "Other")
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	old := seedCoreVisibilityRun(t, env, 1, job, "failed", at)
	cancelled := seedCoreVisibilityRun(t, env, 2, job, "cancelled", at)
	newer := seedCoreVisibilityRun(t, env, 3, job, "failed", at)
	cursor := seedCoreVisibilityRun(t, env, 4, job, "succeeded", at.Add(time.Second))
	seedCoreVisibilityRun(t, env, 5, otherJob, "failed", at.Add(time.Second))
	foreignJob := coreVisibilityJob(t, env, other.ID, "Inspect")
	foreign := seedCoreVisibilityRun(t, env, 6, foreignJob, "failed", at)
	base := fmt.Sprintf("/api/v1/projects/%d/cron/runs?job_uid=%s&limit=2", project.ID, job.UID)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"&status=failed&status=cancelled&status=failed", []string{newer, cancelled}},
		{"&status=failed&status=cancelled&before_uid=" + cancelled, []string{old}},
		{"&status=failed&before_uid=" + cursor, []string{newer, old}},
	} {
		status, raw := cronHTTP(t, env, http.MethodGet, base+tc.query, nil)
		require.Equalf(t, 200, status, "%s", raw)
		var response struct {
			Runs []db.CronRun `json:"runs"`
		}
		require.NoError(t, json.Unmarshal(raw, &response))
		got := make([]string, len(response.Runs))
		for i, run := range response.Runs {
			got[i] = run.UID
		}
		require.Equal(t, tc.want, got, "run status filters must apply before pagination")
	}
	status, raw := cronHTTP(t, env, http.MethodGet, base+"&status=failed&before_uid="+foreign, nil)
	require.Equalf(t, 404, status, "%s", raw)
}

func TestCronCoreRejectsInvalidStatuses(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	for _, endpoint := range []string{"jobs", "runs"} {
		for _, invalid := range []string{"", "FAILED", "failed') OR 1=1 --", "not-a-status"} {
			status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/%s?status=failed&status=%s", project.ID, endpoint, url.QueryEscape(invalid)), nil)
			require.Containsf(t, []int{400, 422}, status, "invalid run status filters must be rejected: %s: %s", endpoint, raw)
		}
	}
	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs?status=never_ran", project.ID), nil)
	require.Containsf(t, []int{400, 422}, status, "%s", raw)
}

// Full text draws exercise the reject direction alongside explicit valid
// status coverage; no input is filtered out because it is hostile.
func TestCronCoreStatusValidationProperty(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	base := fmt.Sprintf("/api/v1/projects/%d/cron", project.ID)
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Text())
		if hegel.Draw(ht, hegel.Booleans()) {
			value = hegel.Draw(ht, hegel.SampledFrom([]string{"running", "succeeded", "failed", "cancelled", "unknown", "never_ran"}))
		}
		endpoint := hegel.Draw(ht, hegel.SampledFrom([]string{"jobs", "runs"}))
		status, raw := cronHTTP(t, env, http.MethodGet, base+"/"+endpoint+"?status="+url.QueryEscape(value), nil)
		valid := value == "running" || value == "succeeded" || value == "failed" || value == "cancelled" || value == "unknown" || endpoint == "jobs" && value == "never_ran"
		if valid {
			require.Equalf(ht, 200, status, "%s", raw)
		} else {
			require.Containsf(ht, []int{400, 422}, status, "%s", raw)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

// Summary uses one daemon-clock instant and includes both created-at window
// boundaries. It must remain an ordinary scoped read in restricted profiles.
func TestCronCoreSummaryRestrictedProfile(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 123456789, time.UTC)
	var clockCalls int
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	other := seedProject(t, env, "other-project")
	job := coreVisibilityJob(t, env, project.ID, "Inspect")
	for i, at := range []time.Time{now.Add(-24*time.Hour - time.Nanosecond), now.Add(-24 * time.Hour), now, now.Add(time.Nanosecond)} {
		seedCoreVisibilityRun(t, env, i+1, job, "failed", at)
	}
	foreignJob := coreVisibilityJob(t, env, other.ID, "Inspect")
	seedCoreVisibilityRun(t, env, 10, foreignJob, "failed", now)
	host := &capabilitiesReadHost{projectID: project.ID}
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: host, EmbeddingProfile: daemon.EmbeddingProfile("restricted"), UIClock: func() time.Time { clockCalls++; return now }})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	for _, tc := range []struct {
		project int64
		want    int
	}{{project.ID, 200}, {other.ID, 404}} {
		request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/summary", tc.project), nil)
		request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "reader", Actor: "reader"}))
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		require.Equalf(t, tc.want, recorder.Code, "summary must be available to an authorized restricted-profile reader: %s", recorder.Body.String())
		if tc.want == 200 {
			var body struct {
				Failed int64     `json:"failed_runs_24h"`
				Start  time.Time `json:"window_start"`
				End    time.Time `json:"window_end"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
			require.EqualValues(t, 2, body.Failed)
			require.Equal(t, now.Add(-24*time.Hour), body.Start)
			require.Equal(t, now, body.End)
		}
	}
	require.Equal(t, 1, clockCalls, "capture one instant for the admitted summary read")
	require.Len(t, host.calls, 2)
	require.Equal(t, "getCronSummary", host.calls[0].ID)
	require.False(t, host.calls[0].Policy.Mutation)
}
