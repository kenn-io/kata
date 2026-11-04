package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/testfix"
	"hegel.dev/go/hegel"
)

func executeCronFleetCLI(t *testing.T, base string, args ...string) (string, error) {
	t.Helper()
	resetFlags(t)
	command := newRootCmd()
	var out, diagnostic bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&diagnostic)
	command.SetContext(contextWithBaseURL(t.Context(), base))
	command.SetArgs(append([]string{"--json", "cron"}, args...))
	err := command.Execute()
	return out.String(), err
}

func TestCronFleetCLIGlobalAndFilters(t *testing.T) {
	t.Setenv("KATA_TEAMMATE", "inherited-runner")
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Inspect", Actor: "operator", Definition: cronCLIJob(t).Definition.Native()})
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Inspect", Author: "operator"})
	require.NoError(t, err)
	id := cronCLIUID(t)
	label, handle := "executor-a", "runner"
	_, err = env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: p.ID, UID: id, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, IssueUID: &issue.UID, ExecutorLabel: &label, Teammate: &handle, Actor: "operator", Status: "failed", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	for _, args := range [][]string{
		{"job", "list", "--all"},
		{"run", "list", "--all"},
		{"run", "list", "--all", "--status", "failed", "--executor-label", label, "--teammate", handle, "--since", "24h", "--issue", strings.ToLower(issue.UID)},
		{"run", "list", "--all", "--issue", p.Name + "#" + issue.ShortID},
	} {
		out, err := executeCronFleetCLI(t, env.URL, args...)
		require.NoError(t, err)
		if args[0] == "run" {
			require.Contains(t, out, id)
		} else {
			require.Contains(t, out, job.UID)
		}
	}
	_, _, _, err = env.DB.SoftDeleteIssue(t.Context(), issue.ID, "operator")
	require.NoError(t, err)
	retained, err := executeCronFleetCLI(t, env.URL, "run", "list", "--all", "--issue", issue.UID)
	require.NoError(t, err)
	require.Contains(t, retained, id)
	_, err = executeCronFleetCLI(t, env.URL, "run", "list", "--all", "--project", p.Name)
	require.ErrorContains(t, err, "--all")
}

func TestCronFleetRunListResolvesIssueWithinSelectedProject(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = env.DB.AttachAlias(t.Context(), project.ID, "github.com/example/spoke-project", "git")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: cronCLIJob(t).Definition.Native()})
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Inspect", Author: "operator"})
	require.NoError(t, err)
	runUID := cronCLIUID(t)
	_, err = env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, IssueUID: &issue.UID, Actor: "operator", Status: "failed", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)

	workspace := testfix.InitGitRepo(t)
	testfix.RunGit(t, workspace, "remote", "add", "origin", "https://github.com/example/spoke-project.git")
	// Confirm the Git alias selects the project without a .kata.toml binding.
	allRuns := runCLI(t, env, workspace, "cron", "run", "list")
	require.Contains(t, allRuns, runUID)

	for _, tc := range []struct {
		name    string
		project string
	}{{name: "Git alias workspace"}, {name: "numeric project selector", project: strconv.FormatInt(project.ID, 10)}} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"cron", "run", "list", "--issue", issue.ShortID}
			if tc.project != "" {
				args = append([]string{"--project", tc.project}, args...)
			}
			out := runCLI(t, env, workspace, args...)
			require.Contains(t, out, runUID)
		})
	}
}

func TestCronFleetCLIOldDaemonAndAuthorization(t *testing.T) {
	for _, tc := range []struct {
		version string
		code    int
		want    string
	}{{"0.38.0", 200, "cross-project cron"}, {"0.39.0", 404, "resource not found"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/health" {
				raw, _ := json.Marshal(map[string]any{"ok": true, "api_schema_version": tc.version})
				_, _ = w.Write(raw)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.code)
			if tc.code == 404 {
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"resource not found"}}`))
			} else {
				_, _ = w.Write([]byte(`{"runs":[],"projects":[]}`))
			}
		}))
		t.Cleanup(server.Close)
		_, err := executeCronFleetCLI(t, server.URL, "run", "list", "--all")
		require.ErrorContains(t, err, tc.want)
	}
}

// Duration conversion is a client-side instant calculation with an explicit clock.
func TestCronFleetSinceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		nanos := hegel.Draw(ht, hegel.Integers(1, int(^uint(0)>>1)))
		now := time.Unix(int64(hegel.Draw(ht, hegel.Integers(1000000000, 2000000000))), 123).UTC()
		got, err := cronSinceInstant(time.Duration(nanos).String(), now)
		require.NoError(ht, err)
		require.True(ht, got.Equal(now.Add(-time.Duration(nanos))))
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestCronFleetCLIFilterEchoSkew(t *testing.T) {
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cron/runs") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"runs":[]}`))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	_, _, err = executeCronCLI(t, server.URL, nil, "run", "list")
	require.NoError(t, err)
	_, _, err = executeCronCLI(t, server.URL, nil, "run", "list", "--status", "failed")
	require.ErrorContains(t, err, "daemon does not support cron run filters")
}

func TestCronFleetRejectsExplicitEmptyStatus(t *testing.T) {
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	for _, args := range [][]string{{"run", "list", "--status", ""}, {"run", "list", "--status", "failed", "--status", ""}} {
		_, _, err := executeCronCLI(t, env.URL, nil, args...)
		require.ErrorContains(t, err, "status")
	}
}
