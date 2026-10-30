package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type dueListOutput struct {
	NextCursor string `json:"next_cursor"`
	Total      *int64 `json:"total"`
	Issues     []struct {
		Title           string `json:"title"`
		ScheduledOnDate string `json:"scheduled_on_date"`
		DeadlineOnDate  string `json:"deadline_on_date"`
	} `json:"issues"`
}

func (o dueListOutput) titles() []string {
	var out []string
	for _, row := range o.Issues {
		out = append(out, row.Title)
	}
	slices.Sort(out)
	return out
}

func parseDueList(t *testing.T, out string) dueListOutput {
	t.Helper()
	var parsed dueListOutput
	require.NoError(t, json.Unmarshal([]byte(out), &parsed), out)
	return parsed
}

func createDueIssue(t *testing.T, env *testenv.Env, projectID int64, title string, meta map[string]string, owner string, labels ...string) db.Issue {
	t.Helper()
	values := map[string]jsontext.Value{}
	for key, value := range meta {
		values[key] = jsontext.Value(fmt.Sprintf("%q", value))
	}
	params := db.CreateIssueParams{
		ProjectID: projectID, Title: title, Author: "tester", Labels: labels, Metadata: values,
	}
	if owner != "" {
		params.Owner = &owner
	}
	issue, _, err := env.DB.CreateIssue(t.Context(), params)
	require.NoError(t, err)
	return issue
}

func TestList_DueByFiltersByTodayRuleAndReturnsDates(t *testing.T) {
	t.Setenv("TZ", "UTC")
	env, dir, pid := setupCLIWorkspace(t)
	createDueIssue(t, env, pid, "date scheduled", map[string]string{"scheduled_on": "2026-11-02"}, "")
	createDueIssue(t, env, pid, "date deadline", map[string]string{"deadline_on": "2026-11-01"}, "")
	createDueIssue(t, env, pid, "local time", map[string]string{"scheduled_on": "2026-11-02T09:00"}, "")
	createDueIssue(t, env, pid, "rfc3339", map[string]string{"deadline_on": "2026-11-02T23:30:00Z"}, "")
	createDueIssue(t, env, pid, "future", map[string]string{"scheduled_on": "2026-11-03"}, "")
	createDueIssue(t, env, pid, "undated", nil, "")
	closed := createDueIssue(t, env, pid, "closed", map[string]string{"scheduled_on": "2026-11-01"}, "")
	_, _, _, err := env.DB.CloseIssue(t.Context(), closed.ID, "done", "tester", "", nil)
	require.NoError(t, err)

	got := parseDueList(t, runCLI(t, env, dir, "--json", "list", "--due-by", "2026-11-02"))
	assert.Equal(t, []string{"date deadline", "date scheduled", "local time", "rfc3339"}, got.titles(),
		"closed issues are left out by default")
	for _, row := range got.Issues {
		switch row.Title {
		case "date scheduled":
			assert.Equal(t, "2026-11-02", row.ScheduledOnDate)
		case "date deadline":
			assert.Equal(t, "2026-11-01", row.DeadlineOnDate)
		case "rfc3339":
			assert.Equal(t, "2026-11-02", row.DeadlineOnDate)
		}
	}

	withClosed := parseDueList(t, runCLI(t, env, dir, "--json", "list", "--status", "all", "--due-by", "2026-11-02"))
	assert.Contains(t, withClosed.titles(), "closed")

	agent := runCLI(t, env, dir, "--agent", "list", "--due-by", "2026-11-02")
	assert.Contains(t, agent, "scheduled_on_date=2026-11-02")
	assert.Contains(t, agent, "deadline_on_date=2026-11-01")
}

func TestList_DueByReadsTheDateInTheLocalZone(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createDueIssue(t, env, pid, "late utc", map[string]string{"deadline_on": "2026-11-02T23:30:00Z"}, "")

	t.Setenv("TZ", "UTC")
	assert.Equal(t, []string{"late utc"},
		parseDueList(t, runCLI(t, env, dir, "--json", "list", "--due-by", "2026-11-02")).titles())

	t.Setenv("TZ", "Asia/Tokyo")
	assert.Empty(t, parseDueList(t, runCLI(t, env, dir, "--json", "list", "--due-by", "2026-11-02")).titles(),
		"the same instant is already the 3rd in Tokyo")
	tokyo := parseDueList(t, runCLI(t, env, dir, "--json", "list", "--due-by", "2026-11-03"))
	require.Len(t, tokyo.Issues, 1)
	assert.Equal(t, "2026-11-03", tokyo.Issues[0].DeadlineOnDate)
}

func TestList_AllDueByComposesWithOwnerAndLabels(t *testing.T) {
	t.Setenv("TZ", "UTC")
	env, _, pid := setupCLIWorkspace(t)
	secondary, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	due := map[string]string{"scheduled_on": "2026-11-01"}
	createDueIssue(t, env, pid, "primary match", due, "alice", "tracked")
	createDueIssue(t, env, secondary.ID, "secondary match", due, "alice", "tracked")
	createDueIssue(t, env, secondary.ID, "wrong owner", due, "bob", "tracked")
	createDueIssue(t, env, secondary.ID, "wrong label", due, "alice", "other")
	createDueIssue(t, env, secondary.ID, "not due", map[string]string{"scheduled_on": "2026-12-01"}, "alice", "tracked")
	closed := createDueIssue(t, env, secondary.ID, "closed match", due, "alice", "tracked")
	_, _, _, err = env.DB.CloseIssue(t.Context(), closed.ID, "done", "tester", "", nil)
	require.NoError(t, err)

	out, err := runCmdOutput(t, env, "--json", "list", "--all",
		"--due-by", "2026-11-02", "--owner", "alice", "--label", "tracked")
	require.NoError(t, err)
	got := parseDueList(t, out)
	assert.Equal(t, []string{"primary match", "secondary match"}, got.titles(),
		"--all widens the project scope; closed issues stay out by default")

	out, err = runCmdOutput(t, env, "--json", "list", "--all", "--status", "all",
		"--due-by", "2026-11-02", "--owner", "alice", "--label", "tracked")
	require.NoError(t, err)
	assert.Equal(t, []string{"closed match", "primary match", "secondary match"}, parseDueList(t, out).titles())
}

func TestList_DueByLimitTotalAndCursorCountMatchingRows(t *testing.T) {
	t.Setenv("TZ", "UTC")
	env, dir, pid := setupCLIWorkspace(t)
	for i := range 3 {
		createDueIssue(t, env, pid, fmt.Sprintf("due %d", i), map[string]string{"scheduled_on": "2026-11-01"}, "")
	}
	for i := range 3 {
		createDueIssue(t, env, pid, fmt.Sprintf("newer %d", i), nil, "")
	}

	first := parseDueList(t, runCLI(t, env, dir, "--json", "list", "--due-by", "2026-11-02",
		"--sort", "oldest", "--limit", "2", "--include-total"))
	assert.Equal(t, []string{"due 0", "due 1"}, first.titles())
	require.NotNil(t, first.Total)
	assert.Equal(t, int64(3), *first.Total, "total counts matching rows, not the limit")
	require.NotEmpty(t, first.NextCursor)

	second := parseDueList(t, runCLI(t, env, dir, "--json", "list", "--due-by", "2026-11-02",
		"--sort", "oldest", "--limit", "2", "--cursor", first.NextCursor))
	assert.Equal(t, []string{"due 2"}, second.titles())

	_, err := runCLICapture(t, env, dir, "--json", "list", "--due-by", "2026-11-03",
		"--sort", "oldest", "--limit", "2", "--cursor", first.NextCursor)
	require.Error(t, err, "a cursor from another due-by date is refused")
}

func TestList_DueByRequiresAValidDate(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	for _, args := range [][]string{
		{"list", "--due-by"},
		{"list", "--due-by", ""},
		{"list", "--due-by", "tomorrow"},
		{"list", "--due-by", "2026-11-02T09:00"},
		{"list", "--due", "2026-11-02"},
	} {
		_, err := runCLICapture(t, env, dir, args...)
		require.Errorf(t, err, "kata %v", args)
	}
	_, err := runCLICapture(t, env, dir, "list", "--due-by", "tomorrow")
	_ = requireCLIError(t, err, ExitValidation)
}
