package daemon_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronDispatchExplanationProjectScopedRead(t *testing.T) {
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	other, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Inspect", Actor: "operator", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	host := &capabilitiesReadHost{projectID: p.ID}
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: host})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	for _, tc := range []struct {
		id   int64
		uid  string
		want int
	}{{p.ID, job.UID, 200}, {other.ID, job.UID, 404}, {p.ID, "01K5ZN7SP700000000000000001", 404}} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/dispatch", tc.id, tc.uid), nil)
		request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "reader", Actor: "reader"}))
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		require.Equalf(t, tc.want, response.Code, "%s", response.Body.String())
		if tc.want == 200 {
			require.Contains(t, response.Body.String(), `"reason":"external_adapter"`)
			require.Contains(t, response.Body.String(), `"last":null`)
		}
	}
	require.Len(t, host.calls, 3)
	require.Equal(t, "explainCronJobDispatch", host.calls[0].ID)
	require.False(t, host.calls[0].Policy.Mutation)
}

type failingExplanationStore struct{ db.Storage }

func (s failingExplanationStore) ExplainRecurringCron(context.Context, int64, string, time.Time) (db.RecurringExplanation, error) {
	return db.RecurringExplanation{Reason: cron.DispatchError, Detail: cron.DispatchError.PublicDetail()}, errors.New("local-sensitive-detail")
}

type archiveBeforeExplanationStore struct {
	db.Storage
	archive func() error
}

func (s archiveBeforeExplanationStore) ExplainRecurringCron(ctx context.Context, projectID int64, jobUID string, now time.Time) (db.RecurringExplanation, error) {
	if err := s.archive(); err != nil {
		return db.RecurringExplanation{}, err
	}
	return s.Storage.ExplainRecurringCron(ctx, projectID, jobUID, now)
}

func TestCronDispatchExplanationProjectArchivedAfterPreflight(t *testing.T) {
	for _, role := range []string{"local", "spoke", "hub"} {
		t.Run(role, func(t *testing.T) {
			env := testenv.New(t)
			p, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Inspect", Actor: "operator", Definition: cronHTTPDefinition(t)})
			require.NoError(t, err)
			if role != "local" {
				_, err = env.DB.EnableProjectFederation(t.Context(), p.ID, "operator")
				require.NoError(t, err)
				_, err = env.DB.ExecContext(t.Context(), `UPDATE federation_bindings SET role=$1,enabled=0,hub_project_id=1,hub_url='https://daemon.example' WHERE project_id=$2`, role, p.ID)
				require.NoError(t, err)
			}
			server := daemon.NewServer(daemon.ServerConfig{DB: archiveBeforeExplanationStore{Storage: env.DB, archive: func() error {
				// Model a separate writer, outside the read request's transaction fence.
				_, _, err := env.DB.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: p.ID, Actor: "operator", Force: role != "local"})
				return err
			}}})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/dispatch", p.ID, job.UID), nil)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
		})
	}
}

func TestCronDispatchExplanationRedactsFailureAndObservation(t *testing.T) {
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Review", Author: "operator"})
	require.NoError(t, err)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 60}, Action: cron.Action{Kind: "notify", Recipient: "operator"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review"}, Overlap: "allow", Catchup: "all"}
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Review", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	recorder := daemon.NewCronDispatchRecorder(filepath.Join(t.TempDir(), "dispatch.jsonl"))
	t.Cleanup(func() { require.NoError(t, recorder.Close()) })
	_, err = recorder.Record(t.Context(), db.RecurringCronCandidate{JobUID: job.UID, ProjectID: p.ID, ProjectUID: p.UID}, db.DispatchRecurringCronOut{Reason: cron.DispatchError}, errors.New("local-sensitive-detail"), time.Now())
	require.NoError(t, err)
	_, err = recorder.Record(t.Context(), db.RecurringCronCandidate{JobUID: job.UID, ProjectID: p.ID, ProjectUID: p.UID}, db.DispatchRecurringCronOut{Reason: cron.DispatchIdle}, nil, time.Now().Add(time.Minute))
	require.NoError(t, err)
	var logs bytes.Buffer
	server := daemon.NewServer(daemon.ServerConfig{DB: failingExplanationStore{env.DB}, CronRecorder: recorder, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/dispatch", p.ID, job.UID), nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	require.Equalf(t, 200, response.Code, "%s", response.Body.String())
	require.NotContains(t, response.Body.String(), "local-sensitive-detail")
	require.Contains(t, response.Body.String(), cron.DispatchError.PublicDetail())
	require.Contains(t, logs.String(), "local-sensitive-detail")
	require.Contains(t, response.Body.String(), `"last":{`)
	// The route must wire both recorder outputs correctly; recorder-only tests
	// cannot detect a handler that omits or swaps Last and LastNonIdle.
	var body api.CronDispatchExplanation
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.NotNil(t, body.Last)
	require.Equal(t, cron.DispatchIdle, body.Last.Reason)
	require.NotNil(t, body.LastNonIdle)
	require.Equal(t, cron.DispatchError, body.LastNonIdle.Reason)
}

// Early authority gates must still establish project-scoped job existence.
func TestCronDispatchExplanationEarlyGatesCheckProjectMembership(t *testing.T) {
	for _, role := range []string{"spoke", "hub"} {
		t.Run(role, func(t *testing.T) {
			env := testenv.New(t)
			p, err := env.DB.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			other, err := env.DB.CreateProject(t.Context(), "hub-project")
			require.NoError(t, err)
			local, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Local", Actor: "operator", Definition: cronHTTPDefinition(t)})
			require.NoError(t, err)
			foreign, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: other.ID, Name: "Private", Actor: "operator", Definition: cronHTTPDefinition(t)})
			require.NoError(t, err)
			_, err = env.DB.EnableProjectFederation(t.Context(), p.ID, "operator")
			require.NoError(t, err)
			q := env.DB
			_, err = q.ExecContext(t.Context(), `UPDATE federation_bindings SET role=$1,enabled=0,hub_project_id=1,hub_url='https://daemon.example' WHERE project_id=$2`, role, p.ID)
			require.NoError(t, err)
			server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: &capabilitiesReadHost{projectID: p.ID}})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			for _, tc := range []struct {
				uid    string
				status int
			}{{foreign.UID, 404}, {"01K5ZN7SP700000000000000001", 404}, {local.UID, 200}} {
				request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/dispatch", p.ID, tc.uid), nil)
				request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "reader", Actor: "reader"}))
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				require.Equalf(t, tc.status, response.Code, "%s", response.Body.String())
			}
		})
	}
}

func TestCronDispatchExplanationNormalizesJobUID(t *testing.T) {
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Inspect", Actor: "operator", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/dispatch", p.ID, strings.ToLower(job.UID)), nil)
	require.Equalf(t, 200, status, "%s", raw)
	require.Contains(t, string(raw), job.UID)
}

func TestCronDispatchExplanationRedactsMalformedDefinition(t *testing.T) {
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Inspect", Actor: "operator", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	_, err = env.DB.ExecContext(t.Context(), `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, `{"version":1,"local-sensitive-detail":true}`, job.UID)
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/dispatch", p.ID, job.UID), nil)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	var body api.CronDispatchExplanation
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Equal(t, cron.DispatchScheduleInvalid, body.Gate.Reason)
	require.Equal(t, cron.DispatchScheduleInvalid.PublicDetail(), body.Gate.Detail)
	require.NotContains(t, string(raw), "local-sensitive-detail")
}
