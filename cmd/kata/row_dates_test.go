package main

import (
	"encoding/json"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// TestListAndShowJSONCarryPlanningDates pins that the CLI hands the daemon's
// resolved calendar dates through in both --json outputs and leaves the keys
// out for an issue without planning metadata.
func TestListAndShowJSONCarryPlanningDates(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	// 23:30Z on the 29th is already the 30th in the issue's zone.
	dated, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: pid, Title: "dated", Author: "tester",
		Metadata: map[string]jsontext.Value{
			"scheduled_on": jsontext.Value(`"2026-10-29T23:30:00Z"`),
			"deadline_on":  jsontext.Value(`"2026-11-02"`),
			"timezone":     jsontext.Value(`"Asia/Tokyo"`),
		},
	})
	require.NoError(t, err)
	undated, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "undated", Author: "tester"})
	require.NoError(t, err)

	var list struct {
		Issues []map[string]any `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "--json", "list")), &list))
	rows := map[any]map[string]any{}
	for _, row := range list.Issues {
		rows[row["title"]] = row
	}
	require.Len(t, rows, 2)
	assert.Equal(t, "2026-10-30", rows["dated"]["scheduled_on_date"])
	assert.Equal(t, "2026-11-02", rows["dated"]["deadline_on_date"])
	assert.NotContains(t, rows["undated"], "scheduled_on_date")
	assert.NotContains(t, rows["undated"], "deadline_on_date")

	var shown struct {
		Issue map[string]any `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "--json", "show", dated.ShortID)), &shown))
	assert.Equal(t, "2026-10-30", shown.Issue["scheduled_on_date"])
	assert.Equal(t, "2026-11-02", shown.Issue["deadline_on_date"])
	var shownUndated struct {
		Issue map[string]any `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "--json", "show", undated.ShortID)), &shownUndated))
	assert.NotContains(t, shownUndated.Issue, "scheduled_on_date")
	assert.NotContains(t, shownUndated.Issue, "deadline_on_date")
}
