package main

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func TestCronRequestCommands(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "request-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Actor: "operator", Name: "Execute review", Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Inspection"}, Overlap: "forbid", Catchup: "all"}})
	require.NoError(t, err)
	execute := func(args ...string) ([]byte, error) {
		t.Helper()
		resetFlags(t)
		root := newRootCmd()
		var out, diagnostics bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&diagnostics)
		root.SetContext(contextWithBaseURL(t.Context(), env.URL))
		root.SetArgs(append([]string{"--project", p.Name, "--json", "cron"}, args...))
		e := root.Execute()
		return out.Bytes(), e
	}
	raw, err := execute("new-uid")
	require.NoError(t, err)
	var generated struct {
		UID string `json:"uid"`
	}
	require.NoError(t, json.Unmarshal(raw, &generated))
	id := generated.UID
	args := []string{"job", "request-run", job.UID, "--request-uid", id, "--ttl", "60"}
	raw, err = execute(args...)
	require.NoError(t, err, string(raw))
	var first db.RequestCronOut
	require.NoError(t, json.Unmarshal(raw, &first))
	require.Equal(t, id, first.Request.UID)
	require.Equal(t, "operator", first.Request.RequestedBy)
	require.Equal(t, job.UID, first.Request.JobUID)
	raw, err = execute(args...)
	require.NoError(t, err, string(raw))
	var replay db.RequestCronOut
	require.NoError(t, json.Unmarshal(raw, &replay))
	require.True(t, replay.Replayed)
	require.Equal(t, first.Request, replay.Request)
	_, err = execute("job", "request-run", job.UID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "request-uid")
	runID, err := uid.New()
	require.NoError(t, err)
	_, err = env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: p.ID, UID: runID, Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	raw, err = execute("new-uid")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &generated))
	cancelID := generated.UID
	raw, err = execute("run", "request-cancel", runID, "--request-uid", cancelID)
	require.NoError(t, err, string(raw))
	var cancel db.RequestCronOut
	require.NoError(t, json.Unmarshal(raw, &cancel))
	require.Equal(t, "cancel", cancel.Request.Kind)
	require.Equal(t, runID, *cancel.Request.RunUID)
	raw, err = execute("request", "list", "--state", "pending")
	require.NoError(t, err, string(raw))
	var list struct {
		Requests []db.CronRequest `json:"requests"`
	}
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Requests, 2)
	_, err = execute("run", "request-cancel", runID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "request-uid")
	fresh, err := uid.New()
	require.NoError(t, err)
	_, err = execute("job", "request-run", job.UID, "--request-uid", fresh, "--ttl", "59")
	require.Error(t, err)
	require.Contains(t, err.Error(), "ttl_seconds: expected number >= 60")
}
