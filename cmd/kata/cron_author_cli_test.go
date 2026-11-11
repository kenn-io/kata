package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronCLITypedCreateUsesExistingUIDAndBody(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	identity := cronCLIUID(t)
	received := make(chan api.PutCronJobBody, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
			_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/7":
			_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/projects/7/cron/jobs/"+identity:
			raw, err := io.ReadAll(r.Body)
			var body api.PutCronJobBody
			if err != nil || json.Unmarshal(raw, &body) != nil {
				t.Error("invalid request body", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			received <- body
			_, _ = io.WriteString(w, `{"job":{"uid":"`+identity+`","definition_event_uid":"01J00000000000000000000002"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	args := append([]string{"job", "create", "--uid", strings.ToLower(identity)}, cronAuthorFixture()...)
	args = append(args, "--adapter-agent", "example-agent")
	out, diagnostic, err := executeCronCLI(t, server.URL, nil, args...)
	require.NoError(t, err)
	require.Contains(t, out, identity)
	require.Contains(t, diagnostic, "reuse --uid on retry")
	select {
	case got := <-received:
		require.JSONEq(t, `{"adapter":"example-adapter","agent":"example-agent"}`, string(got.Definition.Options))
		want, err := cronAuthorBuild(t, "", cronAuthorFixture())
		require.NoError(t, err)
		want.Actor = "worker"
		// Compare options semantically above; JSON object order is immaterial.
		want.Definition.Options = got.Definition.Options
		require.Equal(t, want, got)
	default:
		t.Fatal("typed creation did not use the existing definition PUT")
	}
}

func TestCronCLIAuthoredInputErrorsPerformNoNetworkRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"job", "create", "--file", "-", "--enabled=false"}, "--file"},
		{[]string{"job", "create", "--file=", "--body="}, "--file"},
		{append([]string{"job", "create", "--file", "-"}, cronAuthorFixture()...), "--file"},
		{[]string{"job", "create", "--name", "Inspection"}, "requires explicit"},
		{append(append([]string{"job", "create"}, cronAuthorFixture()...), "--timezone=Local"), "timezone"},
		{append(append([]string{"job", "create"}, cronAuthorFixture()...), "--every=0.5s"), "whole seconds"},
		{append(append([]string{"job", "create"}, cronAuthorFixture()...), "--adapter-agent="), "--adapter-agent must not be empty"},
		{append(append([]string{"job", "create"}, cronAuthorFixture()...), "--adapter-agent= \t\n"), "--adapter-agent must not be empty"},
	} {
		_, _, err := executeCronCLI(t, server.URL, strings.NewReader(`{}`), tc.args...)
		require.ErrorContains(t, err, tc.want)
	}
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
	_, _, err = executeCronCLI(t, env.URL, nil, args...)
	require.NoError(t, err)
	_, _, err = executeCronCLI(t, env.URL, nil, args...)
	require.Error(t, err, "a retained UID conflicts instead of becoming an update")
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

func TestCronCLICreateRetainsGlobalAgentOutputFlag(t *testing.T) {
	resetFlags(t)
	cmd := newRootCmd()
	create, _, err := cmd.Find([]string{"cron", "job", "create"})
	require.NoError(t, err)
	// --agent is an existing global output selector, including on file create.
	// The adapter's agent choice must not shadow it with a string flag.
	require.NoError(t, create.ParseFlags([]string{"--agent", "--file", "request.json"}))
	selected, err := create.Flags().GetBool("agent")
	require.NoError(t, err)
	require.True(t, selected)
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
	_, _, err = executeCronCLI(t, env.URL, nil, args...)
	require.NoError(t, err)
	stored, err := env.DB.CronJob(t.Context(), project.ID, identity)
	require.NoError(t, err)
	require.Equal(t, parent.UID, stored.Definition.Issue.ParentUID)
	require.False(t, stored.Definition.Enabled)
}

func TestCronCLITypedNativeRejectsInvalidDefinitionAfterParentResolution(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "worker"})
	require.NoError(t, err)
	identity := cronCLIUID(t)
	args := []string{"job", "create", "--uid", identity, "--name", "Review", "--title", "Check in", "--enabled=false", "--catchup", "latest", "--overlap", "allow", "--every", "30s", "--notify", "--parent", parent.ShortID, "--recipient", "worker"}
	_, _, err = executeCronCLI(t, env.URL, nil, args...)
	require.ErrorContains(t, err, "at least 60")
	_, err = env.DB.CronJob(t.Context(), project.ID, identity)
	require.Error(t, err)
}
