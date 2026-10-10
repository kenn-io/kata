package main

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestShowPlanningDatesJSON(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "worker"})
	require.NoError(t, err)
	raw, err := runCmdOutput(t, env, "--project", "spoke-project", "show", issue.ShortID, "--planning-dates", "--json")
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &body))
	require.Equal(t, issue.UID, body["issue_uid"])
	require.Contains(t, body, "scheduled_on")
	require.Nil(t, body["scheduled_on"])
	require.Contains(t, body, "deadline_on")
	require.Nil(t, body["deadline_on"])
	require.NotContains(t, body, "issue")
}
