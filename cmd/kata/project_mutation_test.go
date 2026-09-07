package main

import (
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
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
