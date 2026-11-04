package main

import (
	"encoding/json/v2"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func TestCronBundleImportResumesAfterActualPartialCommit(t *testing.T) {
	env, b := cronImportFixture(t)
	source, err := env.DB.ProjectByName(t.Context(), "spoke-project")
	require.NoError(t, err)
	second, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: source.ID, Name: "Second", Actor: "operator", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}})
	require.NoError(t, err)
	sourceJob, err := env.DB.CronJob(t.Context(), source.ID, b.Jobs[0].UID)
	require.NoError(t, err)
	sourceJob.Definition.Action.WorkflowUID = second.UID
	_, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: source.ID, UID: sourceJob.UID, ExpectedEventUID: sourceJob.DefinitionEventUID, Name: sourceJob.Name, Actor: "operator", Definition: sourceJob.Definition})
	require.NoError(t, err)
	exported, _, err := executeCronCLI(t, env.URL, nil, "export", "--all")
	require.NoError(t, err)
	b, err = cron.ParseBundle([]byte(exported))
	require.NoError(t, err)
	target, err := env.DB.ProjectByName(t.Context(), "target-project")
	require.NoError(t, err)
	plan, err := cron.PlanBundle(b, cron.BundleTarget{ProjectUID: target.UID})
	require.NoError(t, err)
	require.Len(t, plan.Workflows, 2)
	var fault atomic.Bool
	// A transparent test transport injects response loss without adding proxy
	// authority/forwarding headers to the daemon's ordinary local client path.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, err := http.NewRequestWithContext(r.Context(), r.Method, env.URL+r.URL.RequestURI(), r.Body) //nolint:gosec // Destination is the owned fixture daemon; only its relative API path varies.
		require.NoError(t, err)
		request.Header = r.Header.Clone()
		response, err := env.HTTP.Do(request) //nolint:gosec // Destination is the owned fixture daemon.
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		status := response.StatusCode
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/cron/workflows/"+plan.Workflows[1].UID) && status == 200 && fault.CompareAndSwap(false, true) {
			status = 503
			body = []byte(`{"error":{"code":"unavailable","message":"reply lost after commit"}}`)
		}
		maps.Copy(w.Header(), response.Header)
		w.Header().Del("Content-Length")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	raw, err := json.Marshal(b)
	require.NoError(t, err)
	args := []string{"import", "--project", "target-project", "--file", "-"}
	out, _, err := executeCronCLI(t, server.URL, strings.NewReader(string(raw)), args...)
	require.Error(t, err)
	var report bundleImportResult
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Items, 2, out)
	require.Equal(t, "created", report.Items[0].Outcome)
	require.Equal(t, "uncertain", report.Items[1].Outcome)
	workflows, err := env.DB.ListCronWorkflows(t.Context(), db.CronList{ProjectID: target.ID})
	require.NoError(t, err)
	require.Len(t, workflows, 2, "both writes actually committed")
	jobs, err := env.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: target.ID})
	require.NoError(t, err)
	require.Empty(t, jobs)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	out, _, err = executeCronCLI(t, server.URL, strings.NewReader(string(raw)), args...)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Items, 3)
	require.Equal(t, "unchanged", report.Items[0].Outcome)
	require.Equal(t, "unchanged", report.Items[1].Outcome)
	require.Equal(t, "created", report.Items[2].Outcome)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before+1, after, "only the pending job emits a new event")
}
