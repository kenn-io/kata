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

	out := runCLI(t, env, dir, "--project", source.Name, "move", issue.ShortID, target.Name, "--dry")

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

// Contract: the default CLI preview lists all descendants; --single opts out.
func TestMoveCLI_Tree(t *testing.T) {
	for _, single := range []bool{false, true} {
		t.Run(map[bool]string{false: "tree", true: "single"}[single], func(t *testing.T) {
			env, dir, source, target, root := setupMoveCLIProjects(t)
			child, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "tester", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
			require.NoError(t, err)
			args := []string{"--project", source.Name, "move", root.ShortID, target.Name}
			if single {
				args = append(args, "--single")
			}
			preview := runCLI(t, env, dir, append(args, "--dry")...)
			if single {
				assert.NotContains(t, preview, child.ShortID)
			} else {
				assert.Contains(t, preview, child.ShortID)
			}
			runCLI(t, env, dir, args...)
			stored, err := env.DB.IssueByID(t.Context(), child.ID)
			require.NoError(t, err)
			expected := target.ID
			if single {
				expected = source.ID
			}
			assert.Equal(t, expected, stored.ProjectID)
		})
	}
}

func TestMoveCLI_OldReference(t *testing.T) {
	env, dir, source, target, issue := setupMoveCLIProjects(t)
	runCLI(t, env, dir, "--project", source.Name, "move", issue.ShortID, target.Name)
	oldRef := source.Name + "#" + issue.ShortID
	assert.Contains(t, runCLI(t, env, dir, "--project", source.Name, "show", issue.UID), "misfiled issue")
	assert.Contains(t, runCLI(t, env, dir, "show", oldRef), "misfiled issue")
	runCLI(t, env, dir, "edit", oldRef, "--title", "continued work")
	got, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "continued work", got.Title)
}

// Contract: subtree output names every moved issue. Previews qualify each
// issue's current project; moves report each descendant's new reference.
func TestMoveCLI_TreeOutputNamesDescendants(t *testing.T) {
	for _, agent := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "agent"}[agent], func(t *testing.T) {
			env, dir, source, target, root := setupMoveCLIProjects(t)
			other, err := env.DB.CreateProject(t.Context(), "other-project")
			require.NoError(t, err)
			child, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: other.ID, Title: "child elsewhere", Author: "tester", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
			require.NoError(t, err)
			args := []string{"--project", source.Name, "move", root.ShortID, target.Name}
			if agent {
				args = append(args, "--agent")
			}
			preview := runCLI(t, env, dir, append(args, "--dry")...)
			assert.Contains(t, preview, other.Name+"#"+child.ShortID)
			out := runCLI(t, env, dir, args...)
			moved, err := env.DB.IssueByID(t.Context(), child.ID)
			require.NoError(t, err)
			assert.Contains(t, out, target.Name+"#"+moved.ShortID)
			assert.Contains(t, out, child.UID)
		})
	}
}
