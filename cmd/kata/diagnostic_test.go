package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestWhoami_FlagOverride(t *testing.T) {
	out := requireCmdOutput(t, nil, "whoami", "--as", "claude-4.7")
	assert.Contains(t, out, "claude-4.7")
	assert.Contains(t, out, "flag")
}

func TestWhoami_FormatJSONAliasAffectsSuccessfulOutput(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newRootCmd(), "--format", "json", "whoami", "--as", "tester"))

	var got struct {
		KataAPIVersion int    `json:"kata_api_version"`
		Actor          string `json:"actor"`
		Source         string `json:"source"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), "output must be JSON: %s", out)
	assert.Equal(t, 1, got.KataAPIVersion)
	assert.Equal(t, "tester", got.Actor)
	assert.Equal(t, "flag", got.Source)
}

func TestWhoami_AgentIncludesActorAndSource(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newRootCmd(), "--agent", "whoami", "--as", "tester"))
	assert.Equal(t, "OK whoami actor=tester source=flag\n", out)
}

func TestHealth_PrintsSchemaVersion(t *testing.T) {
	env := testenv.New(t)
	out := requireCmdOutput(t, env, "health")
	assert.Contains(t, out, "schema_version="+strconv.Itoa(db.CurrentSchemaVersion()))
}

func TestProjectsList_PrintsKnown(t *testing.T) {
	env := testenv.New(t)
	_ = initBoundWorkspace(t, env.URL, "https://github.com/wesm/kata.git")

	out := requireCmdOutput(t, env, "projects", "list")
	assert.True(t, strings.Contains(out, "kata"))
}

func TestProjectsRename_RenamesProject(t *testing.T) {
	env := testenv.New(t)
	dir := initBoundWorkspace(t, env.URL, "https://github.com/wesm/kata.git")
	projectID := resolvePIDViaHTTP(t, env.URL, dir)

	out := requireCmdOutput(t, env, "projects", "rename", itoa(projectID), "Kata Tracker")
	assert.Contains(t, out, "renamed project #"+itoa(projectID)+" to Kata Tracker")

	show := requireCmdOutput(t, env, "projects", "show", itoa(projectID))
	assert.Contains(t, show, "Kata Tracker")
	assert.NotContains(t, show, "next #")
}

func TestProjectsRename_AcceptsProjectSelector(t *testing.T) {
	env := testenv.New(t)
	_ = initBoundWorkspace(t, env.URL, "https://github.com/wesm/alpha.git")

	out := requireCmdOutput(t, env, "projects", "rename", "alpha", "beta")
	assert.Contains(t, out, "renamed project #")
	assert.Contains(t, out, "to beta")
}

func TestProjectsMerge_MergesSourceSelectorIntoSurvivingTarget(t *testing.T) {
	env := testenv.New(t)
	alpha, beta := setupMergeProjects(t, env)
	created, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: alpha.ID, Title: "carry history forward", Author: "tester",
	})
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "projects", "merge", "alpha", "beta")
	assert.Contains(t, out, "merged project #"+itoa(alpha.ID)+" into #"+itoa(beta.ID))
	assert.Contains(t, out, "moved 1 issue")

	// After merge the issue is in beta; look it up by short_id (stable
	// across the move) rather than by the removed legacy #N counter.
	issue, err := env.DB.IssueByShortID(context.Background(), beta.ID, created.ShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "carry history forward", issue.Title)
	_, err = env.DB.ProjectByID(context.Background(), alpha.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestProjectsMerge_RewritesWorkspaceBindingFromSourceToTarget(t *testing.T) {
	env := testenv.New(t)
	setupMergeProjects(t, env)

	dir := t.TempDir()
	require.NoError(t, config.WriteProjectConfig(dir, "alpha"))

	_ = requireCmdOutput(t, env, "--workspace", dir, "projects", "merge", "alpha", "beta")

	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "beta", cfg.Project.Name)
}

func TestProjectsRename_RejectsBlankName(t *testing.T) {
	_, err := runCmdOutput(t, nil, "projects", "rename", "1", "   ")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "project name must be non-empty")
}

func TestProjectSelector_AmbiguousAcrossNameAndAliasSuffix(t *testing.T) {
	projects := []projectRef{
		{ID: 1, Name: "foo"},
		{ID: 2, Name: "bar", Aliases: []projectAliasRef{{AliasIdentity: "github.com/example/foo"}}},
	}
	_, ok, err := uniqueProjectMatch("foo", projects, projectMatchesSelector)
	require.Error(t, err)
	assert.False(t, ok)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "ambiguous")
	assert.Contains(t, ce.Message, "#1 foo")
	assert.Contains(t, ce.Message, "#2 bar")
}

func TestPluralCount_PluralizesAlias(t *testing.T) {
	assert.Equal(t, "1 alias", pluralCount(1, "alias"))
	assert.Equal(t, "2 aliases", pluralCount(2, "alias"))
}
