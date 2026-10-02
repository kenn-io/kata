package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestAgentHooksLocalExecutableIsPortable(t *testing.T) {
	isolateAgentHookHomes(t)
	workspace := t.TempDir()
	_, err := runHookSetup(t, "install", "codex", "--local", "--workspace", workspace)
	require.NoError(t, err)
	entries, err := inspectAgentHookEntries(agenthook.AgentCodex, filepath.Join(workspace, ".codex", "hooks.json"))
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	for _, entry := range entries {
		executable, err := agentHookCommandExecutable(entry.Command, "linux", false)
		require.NoError(t, err)
		require.Equal(t, "kata", executable)
	}
}

func TestAgentHooksReinstallKeepsOpenCodeAPIWithoutRuntime(t *testing.T) {
	home := isolateAgentHookHomes(t)
	_, marker := installOpenCodeVersionFixture(t, "exit 3")
	_, err := runHookSetup(t, "install", "opencode", "--api", "v1", "--executable", os.Args[0])
	require.NoError(t, err)
	path := filepath.Join(home, ".config", "opencode", "plugin", "kata-user.js")
	before, err := os.ReadFile(path) //nolint:gosec // G304: generated artifact under the temporary agent home.
	require.NoError(t, err)
	out, err := runHookSetup(t, "install", "opencode", "--executable", os.Args[0])
	require.NoError(t, err)
	require.Contains(t, out, "v1")
	after, err := os.ReadFile(path) //nolint:gosec // G304: generated artifact under the temporary agent home.
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.FileExists(t, marker)
}

func TestAgentHooksStatusKeepsUserRowsDistinctFromProjects(t *testing.T) {
	isolateAgentHookHomes(t)
	workspace := t.TempDir()
	_, err := runHookSetup(t, "install", "codex", "--local", "--workspace", workspace)
	require.NoError(t, err)
	out, err := runHookSetup(t, "status", "--workspace", workspace, "--json")
	require.NoError(t, err)
	var report struct {
		Harnesses []map[string]any `json:"harnesses"`
		Workspace struct {
			Harnesses []map[string]any `json:"harnesses"`
		} `json:"workspace"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	seen := map[string]bool{}
	for _, row := range report.Harnesses {
		name := row["harness"].(string)
		require.False(t, seen[name], "one user row per agent")
		seen[name] = true
		require.Equal(t, "user", row["scope"])
		require.Contains(t, row, "user")
	}
	require.NotEmpty(t, report.Workspace.Harnesses)
	for _, row := range report.Workspace.Harnesses {
		require.Equal(t, "project", row["scope"])
		require.NotContains(t, row, "user")
		require.Contains(t, row, "config")
	}
}

func TestAgentHooksStatusReturnsParseFailureWithFileAndPartialReport(t *testing.T) {
	resetFlags(t)
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte("{invalid"), 0600))
	out, diagnostic, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "status", "claude", "--json")
	require.Error(t, err)
	require.Equal(t, ExitInternal, exitCodeForErr(err, true))
	require.Empty(t, out)
	var response struct {
		Error struct {
			Message string                `json:"message"`
			Data    agentHookStatusReport `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(diagnostic), &response))
	require.Contains(t, response.Error.Message, strconv.Quote(path))
	require.Len(t, response.Error.Data.Harnesses, 1)
	require.NotEmpty(t, response.Error.Data.Harnesses[0].InspectionError)
}
