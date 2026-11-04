package main

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronCLIValidateParentWithInvalidDocument(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Periodic review", Author: "operator"})
	require.NoError(t, err)
	for _, raw := range []string{`{"name":"Review"}`, `{"name":"Review","definition":null}`, `{"name":"Review","definition":[]}`, `{"name":"Review","definition":{"issue":{"kind":"existing"}}}`} {
		out, _, err := executeCronCLIWithoutJSON(t, env.URL, strings.NewReader(raw), "job", "validate", "--file", "-", "--parent", parent.ShortID, "--quiet")
		var cliErr *cliError
		require.ErrorAs(t, err, &cliErr)
		require.Equal(t, ExitValidation, cliErr.ExitCode)
		var result db.CronValidation
		require.NoError(t, json.Unmarshal([]byte(out), &result), "document failures still print their staged result")
		require.False(t, result.Valid)
		require.Equal(t, "document", result.Errors[0].Stage)
	}
}
