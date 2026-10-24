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
	"go.kenn.io/kata/pkg/client/generated"
)

func TestFinishMutationRepairsBindingBeforeReportingDecodeError(t *testing.T) {
	decodeErr := errors.New("decode response")
	repaired := ""
	p := &projectMutation{name: "old-project", repair: func(name string) error {
		repaired = name
		return nil
	}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Kata-Project-Name": []string{"renamed-project"}},
	}

	_, err := p.finishMutation(resp, nil, decodeErr)
	require.ErrorIs(t, err, decodeErr)
	assert.Equal(t, "renamed-project", repaired)
}

func TestHistoricalMutationFollowupCommentUsesStableIssueUID(t *testing.T) {
	for _, operation := range []string{"edit", "label add"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("KATA_AUTHOR", "worker")
			env, dir := setupCLIEnv(t)
			source, err := env.DB.CreateProject(t.Context(), "source-project")
			require.NoError(t, err)
			target, err := env.DB.CreateProject(t.Context(), "target-project")
			require.NoError(t, err)
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "moved task", Author: "worker",
			})
			require.NoError(t, err)
			collision, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: target.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "existing target task", Author: "worker",
			})
			require.NoError(t, err)
			require.Equal(t, issue.ShortID, collision.ShortID)
			_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
				IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker",
			})
			require.NoError(t, err)
			moved, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.NotEqual(t, issue.ShortID, moved.ShortID)
			oldRef := source.Name + "#" + issue.ShortID
			if operation == "edit" {
				runCLI(t, env, dir, "edit", oldRef, "--title", "continued work", "--comment", "continuing after relocation")
				updated, err := env.DB.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				assert.Equal(t, "continued work", updated.Title)
			} else {
				runCLI(t, env, dir, "label", "add", oldRef, "example-label", "--comment", "continuing after relocation")
				labels, err := env.DB.LabelsByIssue(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Len(t, labels, 1)
				assert.Equal(t, "example-label", labels[0].Label)
			}
			unrelatedComments, err := env.DB.CommentsByIssue(t.Context(), collision.ID)
			require.NoError(t, err)
			assert.Empty(t, unrelatedComments, "the destination's original short ID belongs to another issue")
			comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1, "the follow-up comment must stay with the mutated issue")
			assert.Equal(t, "continuing after relocation", comments[0].Body)
		})
	}
}

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
	apiClient, err := p.generatedClient()
	require.NoError(t, err)
	response, err := apiClient.CreateIssueWithResponse(t.Context(), &generated.CreateIssueRequestOptions{
		PathParams: &generated.CreateIssuePath{ProjectID: p.selector},
		Body:       &generated.CreateIssueBody{Title: "Example task", Actor: new("tester")},
	})
	require.NoError(t, err)
	_, err = p.finishMutation(response.HTTPResponse, response.Body, nil)
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
