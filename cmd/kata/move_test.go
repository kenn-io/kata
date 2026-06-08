package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func setupMoveCLIProjects(t *testing.T) (*testenv.Env, string, db.Project, db.Project, db.Issue) {
	t.Helper()
	env, dir := setupCLIEnv(t)
	ctx := context.Background()
	source, err := env.DB.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := env.DB.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: source.ID,
		Title:     "misfiled issue",
		Author:    "tester",
	})
	require.NoError(t, err)
	return env, dir, source, target, issue
}

func TestMoveCLI_RoundTrip(t *testing.T) {
	env, dir, source, target, issue := setupMoveCLIProjects(t)

	out := runCLI(t, env, dir, "--project", source.Name, "move", issue.ShortID, target.Name)

	assert.Contains(t, out, source.Name+"#"+issue.ShortID)
	assert.Contains(t, out, target.Name+"#")
	stored, err := env.DB.IssueByUID(context.Background(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, target.ID, stored.ProjectID)
	assert.NotEmpty(t, stored.ShortID)
}

func TestMoveCLI_DryRunDoesNotMoveIssue(t *testing.T) {
	env, dir, source, target, issue := setupMoveCLIProjects(t)

	out := runCLI(t, env, dir, "--project", source.Name, "move", issue.ShortID, target.Name, "--dry-run")

	assert.Contains(t, out, "dry-run")
	assert.Contains(t, out, source.Name+"#"+issue.ShortID)
	assert.Contains(t, out, "project "+target.Name)
	stored, err := env.DB.IssueByID(context.Background(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, source.ID, stored.ProjectID)
	assert.Equal(t, issue.ShortID, stored.ShortID)
}

func TestMoveCLI_WithCommentAppendsToMovedIssue(t *testing.T) {
	env, dir, source, target, issue := setupMoveCLIProjects(t)

	runCLI(t, env, dir, "--project", source.Name, "move", issue.ShortID, target.Name,
		"--comment", "relocated to the target project")

	stored, err := env.DB.IssueByUID(context.Background(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	got := fetchIssueViaHTTPWithComments(t, env, target.ID, stored.ShortID)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "relocated to the target project", got.Comments[0].Body)
}
