package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Explicit contract: the CLI forwards continuation and unset-priority filters,
// and uses daemon completeness instead of guessing from a full page.
func TestCLIDiscoveryPagination(t *testing.T) {
	env, dir, projectID := setupCLIWorkspace(t)
	for range 2 {
		_, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projectID, Title: "matching needle", Author: "example-agent"})
		require.NoError(t, err)
	}
	_, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: projectID, Title: "prioritized task", Author: "example-agent", Priority: new(int64(1)),
	})
	require.NoError(t, err)
	run := func(args ...string) (string, string, error) {
		return executeRootCapture(t, contextWithBaseURL(context.Background(), env.URL), append([]string{"--workspace", dir}, args...)...)
	}
	for _, tool := range []string{"list", "search"} {
		args := []string{"--json", tool}
		if tool == "list" {
			args = append(args, "--sort", "created", "--priority-unset", "--include-total")
		} else {
			args = append(args, "needle", "--lexical")
		}
		args = append(args, "--limit", "1")
		out, stderr, err := run(args...)
		require.NoError(t, err, stderr)
		var first struct {
			Next     string `json:"next_cursor"`
			Complete bool   `json:"complete"`
			Total    int    `json:"total"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &first))
		require.False(t, first.Complete)
		require.NotEmpty(t, first.Next)
		if tool == "list" {
			require.Equal(t, 2, first.Total)
		}
		out, stderr, err = run(append(args, "--cursor", first.Next)...)
		require.NoError(t, err, stderr)
		var last struct {
			Next     string `json:"next_cursor"`
			Complete bool   `json:"complete"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &last))
		require.True(t, last.Complete)
		require.Empty(t, last.Next)
	}
	_, stderr, err := run("list", "--priority-unset", "--limit", "2")
	require.NoError(t, err)
	require.NotContains(t, stderr, "showing 2")
	_, _, err = run("list", "--priority-unset", "--priority", "0")
	require.ErrorContains(t, err, "mutually exclusive")
	_, _, err = run("search", "needle", "--cursor", "invalid")
	require.ErrorContains(t, err, "--lexical")
}
