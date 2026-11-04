package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronBundleImportMappingsAndNativeProgress(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	source, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	target, err := env.DB.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Source parent", Author: "operator"})
	require.NoError(t, err)
	destination, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "Destination parent", Author: "operator"})
	require.NoError(t, err)
	other, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "Other parent", Author: "operator"})
	require.NoError(t, err)
	nativeUID := cronCLIUID(t)
	native := cronCLIRecurringJob(t)
	native.Definition.Issue.ParentUID = parent.UID
	_, _, err = executeCronCLI(t, env.URL, nil, "job", "create", "--uid", nativeUID, "--file", cronCLIFile(t, native))
	require.NoError(t, err)
	adapter := cronCLIJob(t)
	adapter.Definition.Trigger = cron.Trigger{Kind: "issue-scheduled", IssueUID: parent.UID}
	adapter.Definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: parent.UID}
	_, _, err = executeCronCLI(t, env.URL, nil, "job", "create", "--uid", cronCLIUID(t), "--file", cronCLIFile(t, adapter))
	require.NoError(t, err)
	exported, _, err := executeCronCLI(t, env.URL, nil, "export", "--all")
	require.NoError(t, err)
	b, err := cron.ParseBundle([]byte(exported))
	require.NoError(t, err)
	result, _, err := importBundleCLI(t, env, b, "--map-issue", parent.UID+"="+destination.ShortID)
	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	var nativeCopy string
	for _, item := range result.Items {
		stored, err := env.DB.CronJob(t.Context(), target.ID, item.UID)
		require.NoError(t, err)
		if stored.Definition.Action.Kind == "notify" {
			nativeCopy = item.UID
			require.Equal(t, destination.UID, stored.Definition.Issue.ParentUID)
			require.False(t, stored.Definition.Enabled)
		} else {
			require.Equal(t, destination.UID, stored.Definition.Trigger.IssueUID)
			require.Equal(t, destination.UID, stored.Definition.Issue.UID)
		}
	}
	require.NotEmpty(t, nativeCopy)
	destination, err = env.DB.IssueByID(t.Context(), destination.ID)
	require.NoError(t, err)
	var metadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(destination.Metadata), &metadata))
	require.Contains(t, metadata, "cron.progress."+nativeCopy)
	require.NotContains(t, metadata, "cron.progress."+nativeUID)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	for _, ref := range []string{destination.UID, "target-project#" + destination.ShortID} {
		result, _, err = importBundleCLI(t, env, b, "--map-issue", parent.UID+"="+ref)
		require.NoError(t, err)
		for _, item := range result.Items {
			require.Equal(t, "unchanged", item.Outcome)
		}
	}
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, _, err = importBundleCLI(t, env, b, "--replace", "--map-issue", parent.UID+"="+other.UID)
	require.ErrorContains(t, err, "immutable")
	stored, err := env.DB.CronJob(t.Context(), target.ID, nativeCopy)
	require.NoError(t, err)
	require.Equal(t, destination.UID, stored.Definition.Issue.ParentUID)
	_, _, err = importBundleCLI(t, env, b, "--map-issue", parent.UID+"="+destination.UID, "--map-issue", parent.UID+"="+other.UID)
	require.ErrorContains(t, err, "conflicting issue mappings")
	out, _, err := executeCronCLI(t, env.URL, nil, "copy", "--job", nativeUID, "--to-project", "spoke-project")
	require.NoError(t, err)
	var same bundleImportResult
	require.NoError(t, json.Unmarshal([]byte(out), &same))
	require.Len(t, same.Items, 1)
	stored, err = env.DB.CronJob(t.Context(), source.ID, same.Items[0].UID)
	require.NoError(t, err)
	require.Equal(t, parent.UID, stored.Definition.Issue.ParentUID)
}
