package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentguide"
	"go.kenn.io/kata/internal/testenv"
)

// Contract: changing instruction mode replaces only Kata's owned span and a
// repeated installation does not rewrite the file. Hooks keep their CLI text.
func TestInitAgentModeSwitch(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	prefix := "# Local rules\n\nKeep `kata list` in these unrelated notes.\n\n"
	suffix := "\n\n## Other rules\nKeep this exactly."
	path := filepath.Join(dir, "AGENTS.md")
	require.NoError(t, os.WriteFile(path, []byte(prefix+agentsBlockBegin+"\nstale\n"+agentsBlockEnd+suffix), 0o600))
	for _, mode := range []string{"mcp", "cli", "mcp"} {
		_, err := runCmdOutput(t, env, "--workspace", dir, "--project", "spoke-project", "init", "--with-agents", "--agent-mode", mode)
		require.NoError(t, err)
		data, err := os.ReadFile(path) //nolint:gosec // test fixture under t.TempDir
		require.NoError(t, err)
		text := string(data)
		require.True(t, strings.HasPrefix(text, prefix+agentsBlockBegin+"\n"))
		require.True(t, strings.HasSuffix(text, agentsBlockEnd+suffix))
		require.Equal(t, 1, strings.Count(text, agentsBlockBegin))
		body := text[len(prefix)+len(agentsBlockBegin)+1 : len(text)-len(suffix)-len(agentsBlockEnd)]
		if mode == "mcp" {
			require.Equal(t, agentguide.MCPContract, body)
		} else {
			require.Equal(t, agentContractText, body)
		}
		before, err := os.Stat(path)
		require.NoError(t, err)
		_, err = runCmdOutput(t, env, "--workspace", dir, "--project", "spoke-project", "init", "--with-agents", "--agent-mode", mode)
		require.NoError(t, err)
		after, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, before.ModTime(), after.ModTime(), "same-mode installation must be a no-op")
		if runtime.GOOS != "windows" {
			require.Equal(t, os.FileMode(0o600), after.Mode().Perm())
		}
	}
}

func TestInitAgentModeValidationBeforeEffects(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--with-agents", "--agent-mode=invalid"}, "--agent-mode must be cli or mcp"},
		{[]string{"--with-agents", "--agent-mode="}, "--agent-mode must be cli or mcp"},
		{[]string{"--with-agents", "--agent-mode=MCP"}, "--agent-mode must be cli or mcp"},
		{[]string{"--agent-mode=mcp"}, "--agent-mode requires --with-agents"},
		{[]string{"--agent-mode=cli"}, "--agent-mode requires --with-agents"},
	} {
		t.Run(strings.Join(tc.args, "/"), func(t *testing.T) {
			dir := t.TempDir()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(server.Close)
			resetFlags(t)
			cmd := newRootCmd()
			cmd.SetContext(contextWithBaseURL(t.Context(), server.URL))
			cmd.SetArgs(append([]string{"--workspace", dir, "--project", "spoke-project", "init"}, tc.args...))
			require.ErrorContains(t, cmd.Execute(), tc.want)
			require.Zero(t, requests.Load())
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestInitAgentModePathBasedFlow(t *testing.T) {
	dir := t.TempDir() // No local project name: exercise start_path fallback.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"name":"spoke-project"},"created":true}`))
	}))
	t.Cleanup(server.Close)
	resetFlags(t)
	cmd := newRootCmd()
	cmd.SetContext(contextWithBaseURL(t.Context(), server.URL))
	cmd.SetArgs([]string{"--workspace", dir, "init", "--with-agents", "--agent-mode=mcp"})
	require.NoError(t, cmd.Execute())
	data, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under t.TempDir
	require.NoError(t, err)
	require.Contains(t, string(data), agentguide.MCPContract)
}

func TestInitMCPGuidanceSafety(t *testing.T) {
	for _, kind := range []string{"orphan end", "missing end", "duplicate", "reversed", "beads", "symlink agents", "symlink claude"} {
		t.Run(kind, func(t *testing.T) {
			env := testenv.New(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "AGENTS.md")
			original := "# Local rules\n"
			switch kind {
			case "orphan end":
				original += agentsBlockEnd
			case "missing end":
				original += agentsBlockBegin
			case "duplicate":
				original += agentsBlockBegin + "\nold\n" + agentsBlockEnd + "\n" + agentsBlockBegin + "\nold\n" + agentsBlockEnd
			case "reversed":
				original += agentsBlockEnd + "\n" + agentsBlockBegin
			case "beads":
				original += beadsFixtureBlock
			}
			outside := filepath.Join(t.TempDir(), "outside.md")
			if strings.HasPrefix(kind, "symlink") {
				if runtime.GOOS == "windows" {
					t.Skip("symlink privileges vary on Windows")
				}
				require.NoError(t, os.WriteFile(outside, []byte(original), 0o600))
				link := path
				if kind == "symlink claude" {
					link = filepath.Join(dir, "CLAUDE.md")
				}
				require.NoError(t, os.Symlink(outside, link))
			} else {
				require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
			}
			stderr := captureProcessStderr(t, func() {
				_, err := runCmdOutput(t, env, "--workspace", dir, "--project", "spoke-project", "init", "--with-agents", "--agent-mode=mcp")
				require.NoError(t, err) // Existing init reports guidance conflicts as warnings.
			})
			readPath := path
			if strings.HasPrefix(kind, "symlink") {
				readPath = outside
			}
			got, err := os.ReadFile(readPath) //nolint:gosec // test fixture under t.TempDir
			require.NoError(t, err)
			require.Equal(t, original, string(got))
			if kind == "beads" {
				proposal, err := os.ReadFile(path + agentsProposalSuffix) //nolint:gosec // test fixture under t.TempDir
				require.NoError(t, err)
				require.Contains(t, string(proposal), agentguide.MCPContract)
				require.NotContains(t, string(proposal), beadsBlockBeginPrefix)
				captureProcessStderr(t, func() {
					_, err = runCmdOutput(t, env, "--workspace", dir, "--project", "spoke-project", "init", "--with-agents", "--agent-mode=cli")
					require.NoError(t, err)
				})
				again, err := os.ReadFile(path + agentsProposalSuffix) //nolint:gosec // test fixture under t.TempDir
				require.NoError(t, err)
				require.Equal(t, proposal, again, "existing sidecar must never be overwritten")
			} else if kind != "symlink claude" {
				require.NotEmpty(t, stderr)
			}
		})
	}
}

// Contract: an agent or script that reads only init's stdout learns that the
// guidance was not installed, without init failing the project binding.
func TestInitReportsGuidanceConflictInMachineOutput(t *testing.T) {
	const message = "AGENTS.md: agent guidance file has malformed or duplicate Kata markers"
	setup := func(t *testing.T) (*testenv.Env, string) {
		t.Helper()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(agentsBlockEnd+"\n"), 0o600))
		return testenv.New(t), dir
	}
	t.Run("json", func(t *testing.T) {
		env, dir := setup(t)
		out, err := runCmdOutput(t, env, "--workspace", dir, "--project", "spoke-project", "--json", "init", "--with-agents", "--agent-mode=mcp")
		require.NoError(t, err)
		var body struct {
			Warning string `json:"agent_guidance_warning"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &body), out)
		require.Contains(t, body.Warning, message)
	})
	t.Run("agent", func(t *testing.T) {
		env, dir := setup(t)
		out, err := runCmdOutput(t, env, "--workspace", dir, "--project", "spoke-project", "--agent", "init", "--with-agents", "--agent-mode=mcp")
		require.NoError(t, err)
		require.Contains(t, out, "agent_guidance_warning=")
		require.Contains(t, out, "malformed or duplicate Kata markers")
	})
}

// Contract: a conflict is confined to the affected file, so the other guidance
// file still receives the selected mode.
func TestInitGuidanceConflictStillUpdatesOtherFile(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	conflicted := filepath.Join(dir, "AGENTS.md")
	original := "# Local rules\n" + agentsBlockEnd + "\n"
	require.NoError(t, os.WriteFile(conflicted, []byte(original), 0o600))
	claude := filepath.Join(dir, "CLAUDE.md")
	require.NoError(t, os.WriteFile(claude, []byte("# Claude rules\n"), 0o600))
	stderr := captureProcessStderr(t, func() {
		_, err := runCmdOutput(t, env, "--workspace", dir, "--project", "spoke-project", "init", "--with-agents", "--agent-mode=mcp")
		require.NoError(t, err)
	})
	require.Contains(t, stderr, "AGENTS.md")
	got, err := os.ReadFile(conflicted) //nolint:gosec // test fixture under t.TempDir
	require.NoError(t, err)
	require.Equal(t, original, string(got))
	block, err := agentguide.ManagedBlock("mcp")
	require.NoError(t, err)
	got, err = os.ReadFile(claude) //nolint:gosec // test fixture under t.TempDir
	require.NoError(t, err)
	require.Equal(t, "# Claude rules\n\n"+block+"\n", string(got))
}
