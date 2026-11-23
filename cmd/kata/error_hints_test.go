package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

const projectInitHint = `run "kata init --project <name>" in the workspace root, or pass --project <name>`

func TestErrorHintsArchivedProjectRestoreAndInit(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/example/spoke-project.git")
	_, err := runCmdOutput(t, env, "--workspace", dir, "init")
	require.NoError(t, err)
	project, err := env.DB.ProjectByName(t.Context(), "spoke-project")
	require.NoError(t, err)
	_, err = runCmdOutput(t, env, "projects", "remove", "spoke-project")
	require.NoError(t, err)

	_, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", dir, "list")
	require.Error(t, err)
	assert.Contains(t, stderr, fmt.Sprintf(`run "kata projects restore %d"`, project.ID))
	_, err = runCmdOutput(t, env, "projects", "restore", itoa(project.ID))
	require.NoError(t, err)
	projectsBefore, err := env.DB.ListProjects(t.Context())
	require.NoError(t, err)
	_, err = runCmdOutput(t, env, "--workspace", dir, "init", "--project", "spoke-project")
	require.NoError(t, err)
	projectsAfter, err := env.DB.ListProjects(t.Context())
	require.NoError(t, err)
	assert.Len(t, projectsAfter, len(projectsBefore))
	_, err = runCmdOutput(t, env, "--workspace", dir, "list")
	require.NoError(t, err)
	alias, err := env.DB.AliasByIdentity(t.Context(), "github.com/example/spoke-project")
	require.NoError(t, err)
	assert.Equal(t, project.ID, alias.ProjectID)
}

func TestErrorHintsExplicitProjectInGitWorkspace(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/example/remote-project.git")

	_, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", dir, "--project", "chosen-project", "create", "Example task")
	require.Error(t, err)
	assert.Contains(t, stderr, `run "kata projects create" with name set to "chosen-project"`)
}

func TestErrorHintsExplicitProjectInBoundWorkspace(t *testing.T) {
	for _, state := range []string{"missing", "archived"} {
		t.Run(state, func(t *testing.T) {
			setupKataEnv(t)
			t.Setenv("KATA_SERVER", "")
			env := testenv.New(t)
			dir, _ := initLocalBoundWorkspace(t, env, "bound-project")
			var projectID int64
			if state == "archived" {
				project, err := env.DB.CreateProject(t.Context(), "target-project")
				require.NoError(t, err)
				projectID = project.ID
				_, err = runCmdOutput(t, env, "projects", "remove", "target-project")
				require.NoError(t, err)
			}
			ctx := contextWithBaseURL(t.Context(), env.URL)
			_, stderr, err := executeRootCapture(t, ctx, "--workspace", dir, "--project", "target-project", "list")
			require.Error(t, err)
			if state == "missing" {
				assert.Contains(t, stderr, `run "kata projects create" with name set to "target-project"`)
				_, err = runCmdOutput(t, env, "--workspace", dir, "projects", "create", "target-project")
			} else {
				assert.Contains(t, stderr, fmt.Sprintf("hint: run \"kata projects restore %d\"\n", projectID))
				_, err = runCmdOutput(t, env, "--workspace", dir, "projects", "restore", itoa(projectID))
			}
			require.NoError(t, err)
			_, err = runCmdOutput(t, env, "--workspace", dir, "--project", "target-project", "list")
			require.NoError(t, err)
		})
	}
}

func TestErrorHintsUnboundWorkspace(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	env := testenv.New(t)
	workspace := t.TempDir()
	for _, mode := range []string{"human", "agent", "json"} {
		t.Run(mode, func(t *testing.T) {
			stdout, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL),
				"--workspace", workspace, "list", "--format", mode)
			require.Error(t, err)
			assert.Equal(t, ExitNotFound, exitCodeForErr(err, runEEntered))
			assert.Empty(t, stdout)
			switch mode {
			case "human":
				assert.Contains(t, stderr, "\nhint: "+projectInitHint+"\n")
			case "agent":
				assert.Contains(t, stderr, "\nHint: "+projectInitHint+"\n")
				assert.True(t, strings.HasPrefix(stderr, "ERR list not_found:"))
			case "json":
				var got struct {
					Error struct {
						Hint     string `json:"hint"`
						Code     string `json:"code"`
						ExitCode int    `json:"exit_code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(stderr), &got))
				assert.Equal(t, projectInitHint, got.Error.Hint)
				assert.Equal(t, "project_not_initialized", got.Error.Code)
				assert.Equal(t, ExitNotFound, got.Error.ExitCode)
			}
		})
	}
}

func TestErrorHintsOutputAndOmission(t *testing.T) {
	for _, tc := range []struct{ name, field, want string }{
		{"present", `,"hint":"try again"`, "try again"},
		{"absent", "", ""},
		{"empty", `,"hint":""`, ""},
		{"null", `,"hint":null`, ""},
	} {
		for _, mode := range []outputMode{outputHuman, outputAgent, outputJSON} {
			t.Run(fmt.Sprintf("%s/%s", tc.name, mode), func(t *testing.T) {
				body := `{"error":{"code":"idempotency_mismatch","message":"conflict"` + tc.field + `}}`
				var out bytes.Buffer
				emitErrorForMode(&out, apiErrFromBody(http.StatusConflict, []byte(body)), mode, true)
				text := out.String()
				assert.Contains(t, text, "conflict")
				if tc.want == "" {
					assert.NotContains(t, strings.ToLower(text), "hint")
					return
				}
				switch mode {
				case outputHuman:
					assert.Contains(t, text, "\nhint: try again\n")
				case outputAgent:
					assert.Contains(t, text, "\nHint: try again\n")
				case outputJSON:
					assert.Contains(t, text, `"hint":"try again"`)
				}
			})
		}
	}
}

func TestErrorHintsAgentKeepsFollowupOnOneLine(t *testing.T) {
	cli := apiErrFromBody(http.StatusNotFound, []byte(`{"error":{"code":"project_not_initialized","message":"missing\nextra message","hint":"try again\nextra hint"}}`))
	var out bytes.Buffer
	emitAgentError(&out, "list", cli)
	assert.Equal(t, "ERR list not_found: missing\nHint: try again\n", out.String())
}

func TestErrorHintsNameCLIFlags(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	env := testenv.New(t)
	dir, _ := initLocalBoundWorkspace(t, env, "example-project")
	ctx := contextWithBaseURL(t.Context(), env.URL)
	newIssue := func(t *testing.T) string {
		return runCLI(t, env, dir, "--quiet", "create", "hint fixture "+t.Name())
	}
	closeArgs := []string{"--done", "--commit", "abc1234", "--message", "Closed the hint fixture after verifying the scenario."}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) []string
		hint  string
	}{
		{"projects remove", func(t *testing.T) []string {
			newIssue(t)
			return []string{"projects", "remove", "example-project"}
		}, "close or purge the open issues first, or pass --force"},
		{"projects detach", func(*testing.T) []string {
			return []string{"projects", "detach", "local://" + dir}
		}, "pass --force to drop it anyway, or attach a replacement alias first"},
		{"close already closed", func(t *testing.T) []string {
			ref := newIssue(t)
			runCLI(t, env, dir, append([]string{"close", ref}, closeArgs...)...)
			return append([]string{"close", ref, "--idempotency-key", "close-retry"}, closeArgs...)
		}, "omit --idempotency-key to accept the current state"},
		{"claim if unowned", func(t *testing.T) []string {
			ref := newIssue(t)
			runCLI(t, env, dir, "--as", "alice", "claim", ref)
			return []string{"--as", "bob", "claim", ref, "--if-unowned"}
		}, "choose another issue, or omit --if-unowned only for a deliberate retry"},
		{"list empty meta key", func(*testing.T) []string {
			return []string{"list", "--meta", "=value"}
		}, "pass --meta key or --meta key=value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--workspace", dir, "--agent"}, tc.setup(t)...)
			_, stderr, err := executeRootCapture(t, ctx, args...)
			require.Error(t, err)
			assert.Contains(t, stderr, "\nHint: "+tc.hint+"\n")
		})
	}
}
