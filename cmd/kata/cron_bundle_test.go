package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronBundleExportClosureAndFiles(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	workflowUID := cronCLIUID(t)
	workflow := api.PutCronWorkflowBody{Name: "Inspect", Definition: api.CronWorkflowDefinition{Version: 1, Steps: []api.CronWorkflowStep{{Key: "inspect", Kind: "command", Command: "git status", Options: api.JSONRawObject(`{"large":9007199254740993}`)}}}}
	_, _, err = executeCronCLI(t, env.URL, nil, "workflow", "create", "--uid", workflowUID, "--file", cronCLIFile(t, workflow))
	require.NoError(t, err)
	var ids []string
	for range 2 {
		id := cronCLIUID(t)
		ids = append(ids, id)
		job := cronCLIJob(t)
		job.Definition.Action = cron.Action{Kind: "execute", WorkflowUID: workflowUID}
		job.Definition.SecretRefs = map[string]string{"Z": "z", "A": "a"}
		_, _, err = executeCronCLI(t, env.URL, nil, "job", "create", "--uid", id, "--file", cronCLIFile(t, job))
		require.NoError(t, err)
	}
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	args := []string{"export", "--job", ids[1], "--job", strings.ToLower(ids[0]), "--job", ids[1]}
	out, diagnostic, err := executeCronCLI(t, env.URL, nil, args...)
	require.NoError(t, err)
	require.Empty(t, diagnostic)
	b, err := cron.ParseBundle([]byte(out))
	require.NoError(t, err)
	require.Equal(t, project.UID, b.Source.ProjectUID)
	require.Len(t, b.Workflows, 1)
	require.Len(t, b.Jobs, 2)
	require.Equal(t, ids[0], b.Jobs[0].UID)
	require.Equal(t, ids[1], b.Jobs[1].UID)
	require.Contains(t, out, "9007199254740993")
	for _, key := range []string{`"author":`, `"revision":`, `"runs":`, `"deleted_at":`, `"project_id":`} {
		require.NotContains(t, out, key)
	}
	again, _, err := executeCronCLI(t, env.URL, nil, "export", "--all")
	require.NoError(t, err)
	var b2 cron.Bundle
	require.NoError(t, json.Unmarshal([]byte(again), &b2))
	b2.Source.ExportedAt = b.Source.ExportedAt
	require.Equal(t, b, b2)
	file := filepath.Join(t.TempDir(), "bundle.json")
	fileOut, _, err := executeCronCLI(t, env.URL, nil, "export", "--all", "--output", file)
	require.NoError(t, err)
	require.Empty(t, fileOut)
	raw, err := os.ReadFile(file) //nolint:gosec // G304: export path is test-owned under t.TempDir.
	require.NoError(t, err)
	_, err = cron.ParseBundle(raw)
	require.NoError(t, err)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	// A failed selection must preserve the previous output file.
	_, _, err = executeCronCLI(t, env.URL, nil, "export", "--job", cronCLIUID(t), "--output", file)
	require.Error(t, err)
	retained, err := os.ReadFile(file) //nolint:gosec // G304: export path is test-owned under t.TempDir.
	require.NoError(t, err)
	require.Equal(t, raw, retained)
}

func TestCronBundleExportSelectionAndTombstones(t *testing.T) {
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	for _, args := range [][]string{{"export"}, {"export", "--all", "--job", cronCLIUID(t)}, {"export", "--job", "bad"}} {
		out, _, err := executeCronCLI(t, env.URL, nil, args...)
		require.Error(t, err)
		require.Empty(t, out)
	}
	out, _, err := executeCronCLI(t, env.URL, nil, "export", "--all")
	require.NoError(t, err)
	b, err := cron.ParseBundle([]byte(out))
	require.NoError(t, err)
	require.Empty(t, b.Jobs)
	require.Empty(t, b.Workflows)
	id := cronCLIUID(t)
	input := api.PutCronWorkflowBody{Name: "Inspect", Definition: api.CronWorkflowDefinition{Version: 1, Steps: []api.CronWorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}}
	out, _, err = executeCronCLI(t, env.URL, nil, "workflow", "create", "--uid", id, "--file", cronCLIFile(t, input))
	require.NoError(t, err)
	var created struct {
		Workflow struct {
			DefinitionEventUID string `json:"definition_event_uid"`
		} `json:"workflow"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	_, _, err = executeCronCLI(t, env.URL, nil, "workflow", "delete", id, "--expected-event-uid", created.Workflow.DefinitionEventUID)
	require.NoError(t, err)
	out, _, err = executeCronCLI(t, env.URL, nil, "export", "--workflow", id)
	require.Error(t, err)
	require.Empty(t, out)
	out, _, err = executeCronCLI(t, env.URL, nil, "export", "--all")
	require.NoError(t, err)
	require.NotContains(t, out, id)
}

func TestCronBundleExportUnboundRemoteWorkspaceDoesNotSendPath(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("fixture-token"))
	server, recorder := projectResolutionProxy(t, env, "")
	workspace := t.TempDir()
	t.Chdir(workspace)
	selection := selectProjectResolutionRemote(t, env, workspace, server.URL, "environment")
	args := append([]string{"cron", "export", "--all", "--json"}, selection...)
	_, _, err := executeRootCapture(t, t.Context(), args...)
	require.Empty(t, recorder.take(), "remote project resolution must not receive client workspace paths")
	assertCLIErrorMatches(t, err, unboundRemoteProjectError())
}

func TestCronBundleExportDeterministicMaps(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	job := cronCLIJob(t)
	job.Definition.SecretRefs = map[string]string{"last": "z", "first": "a"}
	_, _, err = executeCronCLI(t, env.URL, nil, "job", "create", "--uid", cronCLIUID(t), "--file", cronCLIFile(t, job))
	require.NoError(t, err)
	var previous string
	for range 8 {
		out, _, err := executeCronCLI(t, env.URL, nil, "export", "--all")
		require.NoError(t, err)
		var fields map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(out), &fields))
		delete(fields, "source")
		raw, err := json.Marshal(fields, json.Deterministic(true))
		require.NoError(t, err)
		if previous != "" {
			require.Equal(t, previous, string(raw))
		}
		previous = string(raw)
	}
}
