package main

import (
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCreateRepairsRenamedWorkspaceAfterInlineResolution(t *testing.T) {
	env := testenv.New(t)
	dir, pid := initLocalBoundWorkspace(t, env, "old-project")
	_, err := env.DB.RenameProject(t.Context(), pid, "renamed-project")
	require.NoError(t, err)
	runCLI(t, env, dir, "create", "Example task", "--body", "Example description")
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "renamed-project", cfg.Project.Name)
}

func TestBareIssueMutationsFollowWorkspaceAlias(t *testing.T) {
	for _, change := range []string{"rename", "merge"} {
		for _, operation := range []string{"edit", "comment", "label"} {
			t.Run(change+"/"+operation, func(t *testing.T) {
				env := testenv.New(t)
				dir, pid := initLocalBoundWorkspace(t, env, "old-project")
				issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: pid, Title: "Example task", Author: "user-a",
				})
				require.NoError(t, err)
				if change == "rename" {
					_, err = env.DB.RenameProject(t.Context(), pid, "current-project")
					require.NoError(t, err)
					// An unrelated project can reuse the old name. The alias
					// must still select the workspace's original project.
					_, err = env.DB.CreateProject(t.Context(), "old-project")
					require.NoError(t, err)
				} else {
					target, err := env.DB.CreateProject(t.Context(), "current-project")
					require.NoError(t, err)
					_, err = env.DB.MergeProjects(t.Context(), db.MergeProjectsParams{
						SourceProjectID: pid, TargetProjectID: target.ID,
					})
					require.NoError(t, err)
				}
				switch operation {
				case "edit":
					runCLI(t, env, dir, "edit", issue.ShortID, "--body", "Updated description")
					updated, err := env.DB.IssueByID(t.Context(), issue.ID)
					require.NoError(t, err)
					assert.Equal(t, "Updated description", updated.Body)
				case "comment":
					runCLI(t, env, dir, "comment", issue.ShortID, "--body", "Example comment")
					comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
					require.NoError(t, err)
					require.Len(t, comments, 1)
					assert.Equal(t, "Example comment", comments[0].Body)
				case "label":
					runCLI(t, env, dir, "label", "add", issue.ShortID, "example-label")
					labels, err := env.DB.LabelsByIssue(t.Context(), issue.ID)
					require.NoError(t, err)
					require.Len(t, labels, 1)
					assert.Equal(t, "example-label", labels[0].Label)
				}
				cfg, err := config.ReadProjectConfig(dir)
				require.NoError(t, err)
				assert.Equal(t, "current-project", cfg.Project.Name)
			})
		}
	}
}

func TestIssueMutationExplicitProjectDoesNotFollowWorkspaceAlias(t *testing.T) {
	for _, selector := range []string{"flag", "qualified"} {
		t.Run(selector, func(t *testing.T) {
			env := testenv.New(t)
			dir, pid := initLocalBoundWorkspace(t, env, "old-project")
			_, err := env.DB.RenameProject(t.Context(), pid, "current-project")
			require.NoError(t, err)
			reused, err := env.DB.CreateProject(t.Context(), "old-project")
			require.NoError(t, err)
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: reused.ID, Title: "Explicit target", Author: "user-a",
			})
			require.NoError(t, err)
			args := []string{"edit", issue.ShortID, "--body", "Updated description"}
			if selector == "flag" {
				args = append(args, "--project", "old-project")
			} else {
				args[1] = "old-project#" + issue.ShortID
				args = append(args, "--project", "current-project")
			}
			runCLI(t, env, dir, args...)
			updated, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			assert.Equal(t, "Updated description", updated.Body)
			cfg, err := config.ReadProjectConfig(dir)
			require.NoError(t, err)
			assert.Equal(t, "old-project", cfg.Project.Name)
		})
	}
}

func TestRelationshipEditFollowsRenamedWorkspaceAlias(t *testing.T) {
	env := testenv.New(t)
	dir, pid := initLocalBoundWorkspace(t, env, "old-project")
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: pid, Title: "Child task", Author: "user-a",
	})
	require.NoError(t, err)
	parent := createIssue(t, env, pid, "Parent task")
	_, err = env.DB.RenameProject(t.Context(), pid, "current-project")
	require.NoError(t, err)
	runCLI(t, env, dir, "edit", issue.UID, "--parent", parent)
	updated := fetchIssueViaHTTP(t, env, pid, issue.ShortID)
	require.Len(t, updated.Links, 1)
	assert.Equal(t, "parent", updated.Links[0].Type)
	assert.Equal(t, parent, updated.Links[0].To.ShortID)
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "current-project", cfg.Project.Name)
}

// Local repair is now after the write. A failure must not suggest that
// creation failed and encourage the caller to create a duplicate issue.
func TestInlineMutationRepairFailureReportsCommittedWrite(t *testing.T) {
	env := testenv.New(t)
	dir, pid := initLocalBoundWorkspace(t, env, "example-project")
	t.Setenv("KATA_SERVER", env.URL)
	t.Chdir(dir)
	resetFlags(t)
	a, err := dialDaemon(t.Context())
	require.NoError(t, err)
	p := &projectMutation{
		api: a, selector: itoa(pid), name: "example-project",
		repair: func(string) error { return os.ErrPermission },
	}
	_, err = p.mutate(http.MethodPost, "/issues", map[string]string{"title": "Example task", "actor": "tester"}, nil)
	require.Error(t, err)
	var cli *cliError
	require.True(t, errors.As(err, &cli), "expected explicit post-commit error, got %v", err)
	assert.Equal(t, "workspace_repair_failed", cli.Code)
	assert.Contains(t, cli.Message, "mutation succeeded")
	assert.Contains(t, cli.Message, "do not repeat")
	// Creation did actually commit; this is not only an error-message test.
	output := runCLI(t, env, dir, "list", "--json")
	assert.Contains(t, output, "Example task")
}
