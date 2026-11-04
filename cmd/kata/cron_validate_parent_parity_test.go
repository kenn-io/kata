package main

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronCLIValidateParentOptionParity(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Parent", Author: "operator"})
	require.NoError(t, err)
	for _, name := range []string{"existing-issue-execute", "issue-scheduled-notify"} {
		t.Run(name, func(t *testing.T) {
			body := cronCLIJob(t)
			body.Definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: parent.UID}
			if name == "issue-scheduled-notify" {
				body.Definition.Trigger = cron.Trigger{Kind: "issue-scheduled", IssueUID: parent.UID}
				body.Definition.Action = cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}
				body.Definition.Issue = nil
			}
			file := cronCLIFile(t, body)
			out, _, err := executeCronCLI(t, env.URL, nil, "job", "validate", "--file", file)
			require.NoError(t, err, out)
			var result db.CronValidation
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			require.True(t, result.Valid, "definition is valid without --parent")
			validDefinition := result.Definition
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			_, _, err = executeCronCLI(t, env.URL, nil, "job", "create", "--uid", cronCLIUID(t), "--file", file, "--parent", parent.ShortID)
			require.ErrorContains(t, err, "--parent requires definition.issue of kind per-run")
			var createErr *cliError
			require.ErrorAs(t, err, &createErr)
			require.Equal(t, ExitValidation, createErr.ExitCode)
			out, _, validateErr := executeCronCLI(t, env.URL, nil, "job", "validate", "--file", file, "--parent", parent.ShortID)
			after, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
			t.Logf("create: exit=%d error=%v; validate: error=%v output=%s", createErr.ExitCode, createErr, validateErr, out)
			var cliErr *cliError
			require.ErrorAs(t, validateErr, &cliErr, "validate must reject an option that create rejects: %s", out)
			require.Equal(t, ExitValidation, cliErr.ExitCode)
			result = db.CronValidation{}
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			require.False(t, result.Valid)
			require.JSONEq(t, string(validDefinition), string(result.Definition))
			require.Nil(t, result.Schedule)
			require.Equal(t, "request", result.Errors[0].Stage)
			require.Contains(t, out, "--parent requires definition.issue of kind per-run")
		})
	}
}
