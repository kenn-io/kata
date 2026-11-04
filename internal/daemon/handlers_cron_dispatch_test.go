package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
		want int
	}{{p.ID, 200}, {other.ID, 404}} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/dispatch", tc.id, job.UID), nil)
		request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "reader", Actor: "reader"}))
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		require.Equalf(t, tc.want, response.Code, "%s", response.Body.String())
		if tc.want == 200 {
			require.Contains(t, response.Body.String(), `"reason":"external_adapter"`)
			require.Contains(t, response.Body.String(), `"last":null`)
		}
	}
	require.Len(t, host.calls, 2)
	require.Equal(t, "explainCronJobDispatch", host.calls[0].ID)
	require.False(t, host.calls[0].Policy.Mutation)
}

type failingExplanationStore struct{ db.Storage }

func (s failingExplanationStore) ExplainRecurringCron(context.Context, int64, string, time.Time) (db.RecurringExplanation, error) {
	return db.RecurringExplanation{Reason: cron.DispatchError, Detail: "local-sensitive-detail"}, errors.New("local-sensitive-detail")
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
	_, err = recorder.Record(t.Context(), db.RecurringCronCandidate{JobUID: job.UID, ProjectUID: p.UID}, db.DispatchRecurringCronOut{Reason: cron.DispatchError}, errors.New("local-sensitive-detail"), time.Now())
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: failingExplanationStore{env.DB}, CronRecorder: recorder})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/dispatch", p.ID, job.UID), nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	require.Equalf(t, 200, response.Code, "%s", response.Body.String())
	require.NotContains(t, response.Body.String(), "local-sensitive-detail")
	require.Contains(t, response.Body.String(), cron.DispatchError.PublicDetail())
	require.Contains(t, response.Body.String(), `"last":{`)
}

// Early authority gates must still establish project-scoped job existence.
func TestCronDispatchExplanationEarlyGatesHideForeignObservation(t *testing.T) {
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
			recorder := daemon.NewCronDispatchRecorder(filepath.Join(t.TempDir(), "dispatch.jsonl"))
			t.Cleanup(func() { require.NoError(t, recorder.Close()) })
			for _, uid := range []string{foreign.UID, local.UID} {
				_, err = recorder.Record(t.Context(), db.RecurringCronCandidate{ProjectUID: other.UID, JobUID: uid, ExpectedEventUID: "private-event"}, db.DispatchRecurringCronOut{Reason: cron.DispatchDelivered, Delivered: 7}, nil, time.Now())
				require.NoError(t, err)
			}
			server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: &capabilitiesReadHost{projectID: p.ID}, CronRecorder: recorder})
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
				require.NotContains(t, response.Body.String(), "private-event")
				if tc.status == 200 {
					require.Contains(t, response.Body.String(), `"last":null`)
				}
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
	for _, suffix := range []string{"", "/dispatch"} {
		status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s%s", p.ID, strings.ToLower(job.UID), suffix), nil)
		require.Equalf(t, 200, status, "%s", raw)
		require.Contains(t, string(raw), job.UID)
	}
}
