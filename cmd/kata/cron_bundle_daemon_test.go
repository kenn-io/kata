package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronBundleNamedDaemonPipeline(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	source := testenv.New(t, testenv.WithAuthToken("source-token"))
	project, err := source.DB.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	workflow, _, err := source.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}})
	require.NoError(t, err)
	jobDefinition := cronCLIJob(t).Definition.Native()
	jobDefinition.Action = cron.Action{Kind: "execute", WorkflowUID: workflow.UID}
	original, _, err := source.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: jobDefinition})
	require.NoError(t, err)
	target := testenv.New(t, testenv.WithAuthToken("target-token"))
	destination, err := target.DB.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	clientHome := t.TempDir()
	t.Setenv("KATA_HOME", clientHome)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("BUNDLE_SOURCE_TOKEN", "source-token")
	t.Setenv("BUNDLE_TARGET_TOKEN", "target-token")
	catalog := fmt.Sprintf("[[daemon]]\nname = \"source-hub\"\nurl = %q\ntoken_env = \"BUNDLE_SOURCE_TOKEN\"\n\n[[daemon]]\nname = \"target-hub\"\nurl = %q\ntoken_env = \"BUNDLE_TARGET_TOKEN\"\n", source.URL, target.URL)
	require.NoError(t, os.WriteFile(filepath.Join(clientHome, "config.toml"), []byte(catalog), 0o600))
	exported, err := runCmdOutput(t, nil, "--daemon", "source-hub", "--project", "source-project", "cron", "export", "--all")
	require.NoError(t, err)
	resetFlags(t)
	command := newRootCmd()
	command.SetContext(t.Context())
	command.SetIn(strings.NewReader(exported))
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"--daemon", "target-hub", "--project", "target-project", "cron", "import", "--file", "-"})
	require.NoError(t, command.Execute(), out.String())
	copied, err := target.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: destination.ID})
	require.NoError(t, err)
	require.Len(t, copied, 1)
	require.NotEqual(t, original.UID, copied[0].UID)
	workflows, err := target.DB.ListCronWorkflows(t.Context(), db.CronList{ProjectID: destination.ID})
	require.NoError(t, err)
	require.Len(t, workflows, 1)
	require.Equal(t, workflows[0].UID, copied[0].Definition.Action.WorkflowUID)
	sourceJobs, err := source.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, sourceJobs, 1)
	require.Equal(t, original.UID, sourceJobs[0].UID)
}
