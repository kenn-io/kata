package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
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

// An incomplete namespace rename breaks real CLI requests or the HTTP routes
// used by clients, even when both components still compile.
func TestCronNamespaceExecutesAgainstDaemon(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	for _, command := range [][]string{
		{"capabilities"},
		{"job", "list"},
		{"workflow", "list"},
		{"run", "list", "--limit", "100"},
	} {
		t.Run(command[0]+fmt.Sprint(command[1:]), func(t *testing.T) {
			resetFlags(t)
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetContext(contextWithBaseURL(t.Context(), env.URL))
			root.SetArgs(append([]string{"--project", "spoke-project", "--json", "cron"}, command...))
			require.NoError(t, root.Execute(), out.String())
			var response map[string]any
			require.NoError(t, json.Unmarshal(out.Bytes(), &response))
			if command[0] == "capabilities" {
				require.Contains(t, response["event_features"], "cron_v1")
			} else {
				require.Contains(t, response, command[0]+"s")
			}
		})
	}
	t.Run("HTTP capabilities", func(t *testing.T) {
		status, body := env.Get(t, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", project.ID))
		require.Equal(t, 200, status, string(body))
		var response map[string]any
		require.NoError(t, json.Unmarshal(body, &response))
		require.Contains(t, response["event_features"], "cron_v1")
	})
}

// A configured native job creates ordinary children and exact inbox requests through the daemon.
func TestCronRecurringChildrenContract(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	project, e := env.DB.CreateProject(t.Context(), "recurring-project")
	require.NoError(t, e)
	parent, _, e := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Ongoing review", Author: "operator"})
	require.NoError(t, e)
	execute := func(args []string, input string) []byte {
		t.Helper()
		resetFlags(t)
		root := newRootCmd()
		var out, diagnostics bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&diagnostics)
		root.SetIn(strings.NewReader(input))
		root.SetContext(contextWithBaseURL(t.Context(), env.URL))
		root.SetArgs(append([]string{"--project", "recurring-project", "--json"}, args...))
		require.NoError(t, root.Execute(), diagnostics.String())
		return out.Bytes()
	}
	request := fmt.Sprintf(`{"name":"Review every two hours","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":7200},"action":{"kind":"notify","recipient":"operator/consumer"},"issue":{"kind":"per-run","parent_uid":%q,"title":"Review service"},"overlap":"allow","catchup":"latest"}}`, parent.UID)
	raw := execute([]string{"cron", "job", "create", "--file", "-"}, request)
	var response struct {
		Job db.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.NotEmpty(t, response.Job.UID)
	parent, e = env.DB.IssueByID(t.Context(), parent.ID)
	require.NoError(t, e)
	var values map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(parent.Metadata), &values))
	var progress cron.RecurringProgress
	require.NoError(t, json.Unmarshal(values["cron.progress."+response.Job.UID], &progress))
	sweep := daemon.NewDueNotificationSweeper(env.DB, daemon.NewEventPublisher(daemon.NewEventBroadcaster(), nil), "UTC")
	now := progress.Anchor.Add(2 * time.Hour)
	require.NoError(t, sweep.RunOnce(t.Context(), now))
	children, e := env.DB.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, e)
	require.Len(t, children, 2)
	var child db.Issue
	for _, issue := range children {
		if issue.ID != parent.ID {
			child = issue
		}
	}
	inboxRefs := func() []string {
		t.Helper()
		var inbox struct {
			Requests []struct {
				Ref string `json:"ref"`
			} `json:"requests"`
		}
		require.NoError(t, json.Unmarshal(execute([]string{"inbox", "--for", "operator/consumer"}, ""), &inbox))
		refs := make([]string, 0, len(inbox.Requests))
		for _, request := range inbox.Requests {
			refs = append(refs, request.Ref)
		}
		return refs
	}
	require.Equal(t, []string{child.ShortID}, inboxRefs())
	execute([]string{"notify", child.ShortID, "--to", "operator/consumer", "--clear"}, "")
	require.NoError(t, sweep.RunOnce(t.Context(), now))
	require.Empty(t, inboxRefs())
	require.NoError(t, sweep.RunOnce(t.Context(), now.Add(2*time.Hour)))
	children, e = env.DB.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, e)
	require.Len(t, children, 3)
	nextRefs := inboxRefs()
	require.Len(t, nextRefs, 1)
	require.NotEqual(t, child.ShortID, nextRefs[0])
	parent, e = env.DB.IssueByID(t.Context(), parent.ID)
	require.NoError(t, e)
	require.Equal(t, "open", parent.Status)
}

// Contract: pause/resume work without a request file and optional CAS applies
// to the selected revision. The response remains the ordinary job JSON.
func TestCronJobEnabledCommands(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Result"}, Overlap: "forbid", Catchup: "skip"}
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	execute := func(action string, extra ...string) (api.CronJobResponse, error) {
		t.Helper()
		resetFlags(t)
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetContext(contextWithBaseURL(t.Context(), env.URL))
		root.SetArgs(append([]string{"--project", "example-project", "--json", "cron", "job", action, job.UID}, extra...))
		err := root.Execute()
		var result api.CronJobResponse
		if err == nil {
			require.NoError(t, json.Unmarshal(out.Bytes(), &result.Body))
		}
		return result, err
	}
	paused, err := execute("pause")
	require.NoError(t, err)
	require.False(t, paused.Body.Job.Definition.Enabled)
	require.Equal(t, "operator", paused.Body.Events[0].Actor)
	_, err = execute("resume", "--expected-event-uid", job.DefinitionEventUID)
	require.Error(t, err)
	resumed, err := execute("resume", "--expected-event-uid", paused.Body.Job.DefinitionEventUID)
	require.NoError(t, err)
	require.True(t, resumed.Body.Job.Definition.Enabled)
}

// Project controls are finite JSON commands, accept no request file, and retain
// the original pause reason on retries.
func TestCronProjectPauseCommands(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	for _, step := range []struct {
		args            []string
		changed, paused bool
	}{
		{[]string{"pause", "--reason", "Maintenance"}, true, true},
		{[]string{"pause", "--reason", "Retry"}, false, true},
		{[]string{"resume"}, true, false},
	} {
		resetFlags(t)
		root := newRootCmd()
		var out, diagnostics bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&diagnostics)
		root.SetContext(contextWithBaseURL(t.Context(), env.URL))
		root.SetArgs(append([]string{"--project", "example-project", "--json", "cron"}, step.args...))
		require.NoError(t, root.Execute(), diagnostics.String())
		var response struct {
			Pause   *db.CronProjectPause `json:"project_pause"`
			Changed bool                 `json:"changed"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &response))
		require.Equal(t, step.changed, response.Changed)
		require.Equal(t, step.paused, response.Pause != nil)
		if response.Pause != nil {
			require.Equal(t, "operator", response.Pause.PausedBy)
			require.Equal(t, "Maintenance", response.Pause.Reason)
		}
	}
}
