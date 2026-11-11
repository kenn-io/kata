package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronCLIAuthoredInputErrorsPerformNoNetworkRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	args := append(append([]string{"job", "create"}, cronAuthorFixture()...), "--every=0.5s")
	_, _, err := executeCronCLI(t, server.URL, nil, args...)
	require.ErrorContains(t, err, "--every must use whole seconds")
	require.Zero(t, requests.Load())
}

func TestCronCLIFileRejectsEveryAuthoredFlag(t *testing.T) {
	for _, arg := range []string{
		"--name=", "--title=", "--body=", "--enabled=false", "--cron=", "--every=", "--once=",
		"--timezone=", "--catchup=", "--overlap=", "--grace=", "--timeout=", "--notify=false",
		"--recipient=", "--message=", "--prompt=", "--adapter=", "--adapter-agent=", "--checkout-key=",
	} {
		_, _, err := executeCronCLI(t, "http://127.0.0.1:1", nil, "job", "create", "--file=-", arg)
		require.ErrorContains(t, err, "mutually exclusive", arg)
	}
}

func TestCronCLITypedCreationHasNoNameUpsert(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	identity := cronCLIUID(t)
	args := append([]string{"job", "create", "--uid", identity}, cronAuthorFixture()...)
	args = append(args, "--adapter= example-adapter ", "--adapter-agent= example-agent ")
	_, diagnostic, err := executeCronCLI(t, env.URL, nil, args...)
	require.NoError(t, err)
	require.Contains(t, diagnostic, "reuse --uid on retry")
	stored, err := env.DB.CronJob(t.Context(), project.ID, identity)
	require.NoError(t, err)
	require.Equal(t, "worker", stored.Author)
	require.JSONEq(t, `{"adapter":"example-adapter","agent":"example-agent"}`, string(stored.Definition.Options))
	_, _, err = executeCronCLI(t, env.URL, nil, append([]string{"job", "create"}, cronAuthorFixture()...)...)
	require.NoError(t, err)
	jobs, err := env.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, jobs, 2, "repeated names create distinct definitions")
	require.Equal(t, jobs[0].Name, jobs[1].Name)
	require.NotEqual(t, jobs[0].UID, jobs[1].UID)
}

func TestCronCLIAuthoringFlagsAreCreateOnly(t *testing.T) {
	for _, args := range [][]string{
		{"job", "update", "01J00000000000000000000001", "--name", "Inspection"},
		{"workflow", "create", "--enabled=false"},
	} {
		_, _, err := executeCronCLI(t, "http://127.0.0.1:1", nil, args...)
		require.ErrorContains(t, err, "unknown flag")
	}
}

func TestCronCLITypedNativeParentResolution(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Ongoing review", Author: "worker"})
	require.NoError(t, err)
	identity := cronCLIUID(t)
	args := []string{"job", "create", "--uid", identity, "--name", "Review", "--title", "Review check-in",
		"--enabled=false", "--catchup", "latest", "--overlap", "allow", "--every", "1h",
		"--notify", "--parent", parent.ShortID, "--recipient", "worker"}
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = target.Host
	}}
	var puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	_, _, err = executeCronCLI(t, server.URL, nil, args...)
	require.NoError(t, err)
	stored, err := env.DB.CronJob(t.Context(), project.ID, identity)
	require.NoError(t, err)
	require.Equal(t, parent.UID, stored.Definition.Issue.ParentUID)
	require.False(t, stored.Definition.Enabled)
	puts.Store(0)
	invalid := append(append([]string{}, args...), "--every=30s", "--uid="+cronCLIUID(t))
	_, diagnostic, err := executeCronCLI(t, server.URL, nil, invalid...)
	require.ErrorContains(t, err, "at least 60")
	require.NotContains(t, diagnostic, "reuse --uid on retry")
	require.Zero(t, puts.Load(), "invalid definition must not reach the write")
}
