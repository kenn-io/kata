package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronJobPreviewCLI(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	p, e := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, e)
	parent, _, e := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Parent", Author: "worker"})
	require.NoError(t, e)
	input := cronCLIRecurringJob(t)
	out, e := executeCronParentCLI(t, env.URL, t.TempDir(), "spoke-project", input, "preview", "--parent", parent.ShortID, "--count", "2", "--from", "2026-03-06T00:00:00Z")
	require.NoError(t, e)
	var body struct {
		Preview struct {
			Occurrences []any `json:"occurrences"`
		} `json:"preview"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	require.Len(t, body.Preview.Occurrences, 2)
	input.Definition.Issue.ParentUID = parent.UID
	job, _, e := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: input.Name, Actor: "worker", Definition: input.Definition.Native()})
	require.NoError(t, e)
	out, _, e = executeCronCLI(t, env.URL, nil, "job", "preview", strings.ToLower(job.UID), "--count", "2")
	require.NoError(t, e)
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	require.Len(t, body.Preview.Occurrences, 2)
}
func TestCronJobPreviewCLIArgumentErrors(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{}, "pass a job UID or --file; use --file - for stdin"},
		{[]string{cronCLIUID(t), "--file", "-"}, "pass a job UID or --file, not both"},
		{[]string{cronCLIUID(t), "--parent", "abcd"}, "--parent applies only to --file"},
		{[]string{cronCLIUID(t), "--count", "0"}, "count must be between 1 and 100"},
		{[]string{cronCLIUID(t), "--from", "9999-12-31T23:00:00-02:00"}, "--from must be an RFC3339 instant"},
		{[]string{cronCLIUID(t), "--from", "bad"}, "--from must be an RFC3339 instant"},
		{[]string{cronCLIUID(t), "--from", "0001-01-01T00:00:00Z"}, "--from must be an RFC3339 instant"},
	} {
		t.Run(tc.message, func(t *testing.T) {
			args := append([]string{"job", "preview"}, tc.args...)
			_, _, err := executeCronCLI(t, "http://127.0.0.1:1", nil, args...)
			var cliErr *cliError
			require.ErrorAs(t, err, &cliErr)
			require.Equal(t, ExitValidation, cliErr.ExitCode)
			require.Equal(t, tc.message, cliErr.Message)
		})
	}
}
func TestCronJobPreviewCLIOldDaemon(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			_, _ = fmt.Fprint(w, `{"api_schema_version":"0.28.0"}`)
			return
		}
		if strings.Contains(r.URL.Path, "/preview") {
			calls.Add(1)
		}
		http.Error(w, "unexpected request", 500)
	}))
	defer server.Close()
	for _, args := range [][]string{{cronCLIUID(t)}, {"--file", "-"}} {
		_, _, err := executeCronCLI(t, server.URL, strings.NewReader(string(mustCronJSON(t, cronCLIRecurringJob(t)))), append([]string{"job", "preview"}, args...)...)
		var cliErr *cliError
		require.ErrorAs(t, err, &cliErr)
		require.Equal(t, "daemon_api_too_old", cliErr.Code)
		require.Contains(t, cliErr.Message, "requires daemon API 0.29.0 or newer")
		require.Zero(t, calls.Load())
	}
}
