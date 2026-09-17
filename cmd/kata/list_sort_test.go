package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestList_SortOldest(t *testing.T) {
	env, dir, projectID := setupCLIWorkspace(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	items := []db.ImportItem{
		{ExternalID: "oldest", Title: "oldest issue", Author: "fixture", Status: "open", CreatedAt: base, UpdatedAt: base},
		{ExternalID: "older-child", Title: "older child", Author: "fixture", Status: "open", CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(10 * time.Hour)},
		{ExternalID: "tie-first", Title: "first tied issue", Author: "fixture", Status: "open", CreatedAt: base.Add(2 * time.Hour), UpdatedAt: base.Add(2 * time.Hour)},
		{ExternalID: "tie-second", Title: "second tied issue", Author: "fixture", Status: "open", CreatedAt: base.Add(2 * time.Hour), UpdatedAt: base.Add(3 * time.Hour)},
		{ExternalID: "newer-parent", Title: "newer parent", Author: "fixture", Status: "open", CreatedAt: base.Add(3 * time.Hour), UpdatedAt: base.Add(3 * time.Hour)},
		{ExternalID: "newest", Title: "newest issue", Author: "fixture", Status: "open", CreatedAt: base.Add(4 * time.Hour), UpdatedAt: base.Add(4 * time.Hour)},
	}
	_, _, err := env.DB.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: projectID, Source: "fixture", Actor: "fixture", Items: items,
	})
	require.NoError(t, err)
	childMapping, err := env.DB.ImportMappingBySource(ctx, projectID, "fixture", "issue", "older-child")
	require.NoError(t, err)
	parentMapping, err := env.DB.ImportMappingBySource(ctx, projectID, "fixture", "issue", "newer-parent")
	require.NoError(t, err)
	require.NotNil(t, childMapping.IssueID)
	require.NotNil(t, parentMapping.IssueID)
	_, err = env.DB.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: *childMapping.IssueID,
		ToIssueID:   *parentMapping.IssueID,
		Type:        "parent",
		Author:      "fixture",
	})
	require.NoError(t, err)

	run := func(args ...string) (string, string, error) {
		return executeRootCapture(t, contextWithBaseURL(ctx, env.URL), append([]string{"--workspace", dir}, args...)...)
	}
	want := []string{
		"oldest issue", "older child", "first tied issue", "second tied issue", "newer parent", "newest issue",
	}

	t.Run("scoped human output is flat", func(t *testing.T) {
		stdout, stderr, err := run("list", "--sort", "oldest")
		require.NoError(t, err, "%s", stderr)
		assertListTitlesInOrder(t, stdout, want)
		assert.NotContains(t, stdout, "├─")
		assert.NotContains(t, stdout, "└─")
	})

	t.Run("all projects", func(t *testing.T) {
		stdout, stderr, err := run("list", "--all", "--sort", "oldest")
		require.NoError(t, err, "%s", stderr)
		assertListTitlesInOrder(t, stdout, want)
		assert.NotContains(t, stdout, "├─")
		assert.NotContains(t, stdout, "└─")
	})

	t.Run("json output", func(t *testing.T) {
		stdout, stderr, err := run("--json", "list", "--sort", "oldest")
		require.NoError(t, err, "%s", stderr)
		assertListJSONTitlesInOrder(t, stdout, want)
	})

	t.Run("agent output", func(t *testing.T) {
		stdout, stderr, err := run("--agent", "list", "--sort", "oldest")
		require.NoError(t, err, "%s", stderr)
		assertListTitlesInOrder(t, stdout, want)
	})

	t.Run("limit is applied after ordering", func(t *testing.T) {
		stdout, stderr, err := run("list", "--sort", "oldest", "--limit", "1")
		require.NoError(t, err, "%s", stderr)
		assert.Contains(t, stdout, "oldest issue")
		assert.NotContains(t, stdout, "older child")
		assert.Contains(t, stderr, "showing 1")
	})
}

func TestList_RejectsInvalidSort(t *testing.T) {
	_, _, err := executeRootCapture(t, context.Background(), "list", "--sort", "newest")
	require.Error(t, err)
	var cliErr *cliError
	require.ErrorAs(t, err, &cliErr)
	assert.Equal(t, ExitValidation, cliErr.ExitCode)
	assert.Contains(t, cliErr.Message, "--sort must be oldest")
}

func assertListTitlesInOrder(t *testing.T, output string, titles []string) {
	t.Helper()
	previous := -1
	for _, title := range titles {
		at := strings.Index(output, title)
		require.Greater(t, at, previous, "title %q was out of order in %q", title, output)
		previous = at
	}
}

func assertListJSONTitlesInOrder(t *testing.T, output string, titles []string) {
	t.Helper()
	var response struct {
		Issues []struct {
			Title string `json:"title"`
		} `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &response))
	got := make([]string, len(response.Issues))
	for i, issue := range response.Issues {
		got[i] = issue.Title
	}
	assert.Equal(t, titles, got)
}
