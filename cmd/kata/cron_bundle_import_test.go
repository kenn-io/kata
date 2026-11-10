package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/testenv"
)

type bundleImportResult struct {
	Items []struct {
		Kind      string `json:"kind"`
		UID       string `json:"uid"`
		SourceUID string `json:"source_uid"`
		Outcome   string `json:"outcome"`
	} `json:"items"`
	DryRun bool `json:"dry_run"`
}

func cronImportFixture(t *testing.T) (*testenv.Env, cron.Bundle) {
	t.Helper()
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	_, err = env.DB.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	workflowUID := cronCLIUID(t)
	workflow := api.PutCronWorkflowBody{Name: "Inspect", Definition: api.CronWorkflowDefinition{Version: 1, Steps: []api.CronWorkflowStep{{Key: "inspect", Kind: "command", Command: "git status", Options: api.JSONRawObject(`{"large":9007199254740993}`)}}}}
	_, _, err = executeCronCLI(t, env.URL, nil, "workflow", "create", "--uid", workflowUID, "--file", cronCLIFile(t, workflow))
	require.NoError(t, err)
	job := cronCLIJob(t)
	job.Definition.Action = cron.Action{Kind: "execute", WorkflowUID: workflowUID}
	job.Definition.Enabled = true
	_, _, err = executeCronCLI(t, env.URL, nil, "job", "create", "--uid", cronCLIUID(t), "--file", cronCLIFile(t, job))
	require.NoError(t, err)
	out, _, err := executeCronCLI(t, env.URL, nil, "export", "--all")
	require.NoError(t, err)
	b, err := cron.ParseBundle([]byte(out))
	require.NoError(t, err)
	return env, b
}

func importBundleCLI(t *testing.T, env *testenv.Env, b cron.Bundle, extra ...string) (bundleImportResult, string, error) {
	t.Helper()
	raw, err := json.Marshal(b)
	require.NoError(t, err)
	args := append([]string{"import", "--file", "-", "--project", "target-project"}, extra...)
	out, _, err := executeCronCLI(t, env.URL, strings.NewReader(string(raw)), args...)
	var result bundleImportResult
	if out != "" {
		require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	}
	return result, out, err
}

func TestCronBundleImportRoundtripRetryDryRunAndCopy(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env, b := cronImportFixture(t)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	predicted, _, err := importBundleCLI(t, env, b, "--dry-run")
	require.NoError(t, err)
	require.True(t, predicted.DryRun)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	result, out, err := importBundleCLI(t, env, b)
	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	require.Equal(t, "workflow", result.Items[0].Kind)
	require.Equal(t, "created", result.Items[0].Outcome)
	require.Equal(t, "created", result.Items[1].Outcome)
	require.NotContains(t, out, `"definition":`)
	shown, _, err := executeCronCLI(t, env.URL, nil, "job", "show", result.Items[1].UID, "--project", "target-project")
	require.NoError(t, err)
	var job struct {
		Job api.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal([]byte(shown), &job))
	require.False(t, job.Job.Definition.Enabled)
	require.Equal(t, result.Items[0].UID, job.Job.Definition.Action.WorkflowUID)
	require.Contains(t, shown, "9007199254740993")
	before, err = env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	retry, _, err := importBundleCLI(t, env, b)
	require.NoError(t, err)
	for _, item := range retry.Items {
		require.Equal(t, "unchanged", item.Outcome)
	}
	dry, _, err := importBundleCLI(t, env, b, "--dry-run")
	require.NoError(t, err)
	for _, item := range dry.Items {
		require.Equal(t, "unchanged", item.Outcome)
	}
	after, err = env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	copied, _, err := executeCronCLI(t, env.URL, nil, "copy", "--job", b.Jobs[0].UID, "--to-project", "target-project")
	require.NoError(t, err)
	require.Contains(t, copied, `"unchanged"`)
}

func TestCronBundleImportConflictReplaceAndWorkflowStop(t *testing.T) {
	env, b := cronImportFixture(t)
	first, _, err := importBundleCLI(t, env, b)
	require.NoError(t, err)
	b.Workflows[0].Name = "Changed workflow"
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	result, _, err := importBundleCLI(t, env, b)
	require.Error(t, err)
	require.Len(t, result.Items, 1)
	require.Equal(t, "conflict", result.Items[0].Outcome)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	result, _, err = importBundleCLI(t, env, b, "--replace", "--dry-run")
	require.NoError(t, err)
	require.Equal(t, "replaced", result.Items[0].Outcome)
	after, err = env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	result, _, err = importBundleCLI(t, env, b, "--replace")
	require.NoError(t, err)
	require.Equal(t, "replaced", result.Items[0].Outcome)
	require.Equal(t, "unchanged", result.Items[1].Outcome)
	shown, _, err := executeCronCLI(t, env.URL, nil, "job", "show", first.Items[1].UID, "--project", "target-project")
	require.NoError(t, err)
	var job struct {
		Job api.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal([]byte(shown), &job))
	_, _, err = executeCronCLI(t, env.URL, nil, "job", "delete", first.Items[1].UID, "--expected-event-uid", job.Job.DefinitionEventUID, "--project", "target-project")
	require.NoError(t, err)
	result, _, err = importBundleCLI(t, env, b, "--replace")
	require.Error(t, err)
	require.Equal(t, "tombstoned", result.Items[len(result.Items)-1].Outcome)
}

func TestCronBundleImportRejectsUnmappedIssuesBeforeWrites(t *testing.T) {
	env, b := cronImportFixture(t)
	var doc map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(b.Jobs[0].Definition, &doc))
	doc["issue"] = jsontext.Value(`{"kind":"existing","uid":"` + cronCLIUID(t) + `"}`)
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	b.Jobs[0].Definition = raw
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	_, _, err = importBundleCLI(t, env, b)
	require.ErrorContains(t, err, "missing issue mappings")
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, _, err = importBundleCLI(t, env, b, "--map-issue", cronCLIUID(t)+"=spoke-project#abc4")
	require.ErrorContains(t, err, "issue mapping must resolve in target project target-project")
	_, _, err = importBundleCLI(t, env, b, "--map-issue", "bad=abc4")
	require.ErrorContains(t, err, "cron identity must be a 26-character ULID")
}
