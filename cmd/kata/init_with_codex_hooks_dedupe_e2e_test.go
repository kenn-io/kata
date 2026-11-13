package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kit/agenthook"
)

func TestE2E_InitWithCodexHooksRunsOwnershipMarkedCommands(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	configPath := filepath.Join(dir, ".codex", "hooks.json")
	foreign := map[string]any{"type": "command", "command": "example-tool contract"}
	data, err := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": []any{
		map[string]any{"hooks": []any{foreign}},
	}}})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o700))
	require.NoError(t, os.WriteFile(configPath, data, 0o600)) //nolint:gosec // Test config lives under the isolated workspace.

	_, _, err = applyCodexHooks(t.Context(), dir)
	require.NoError(t, err)
	ownership := agentHookConfigOwnershipMarkers(agenthook.AgentCodex, configPath, "kata")
	contractOwnership := ownership[contractHook]
	require.True(t, contractOwnership.Required)
	require.NotEmpty(t, contractOwnership.Token)

	ref := createIssue(t, env, pid, "session hook ownership")
	t.Setenv("KATA_REF", ref)
	contract := runCLI(t, env, dir, "agent-contract-hook", "--ownership-marker", agentSetupHookOwnershipMarker(contractHook, contractOwnership.Token))
	var response struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	require.NoError(t, json.Unmarshal([]byte(contract), &response))
	assert.Equal(t, agentContractText, response.HookSpecificOutput.AdditionalContext)
	require.NoError(t, runAttnHook(t, env, dir, "attention-hook", "start"))
	value, exists := attnMetaValue(t, env, pid, ref, attentionKey)
	require.True(t, exists)
	assert.Equal(t, attnValueOK, value)
	require.NoError(t, runAttnHook(t, env, dir, "attention-hook", "end"))
	value, exists = attnMetaValue(t, env, pid, ref, attentionKey)
	require.True(t, exists)
	assert.Equal(t, attnValueNeedsHuman, value)
}

func TestE2E_InitWithCodexHooks_DedupeAndRestore(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/example-org/example-workspace.git")
	t.Setenv("CODEX_HOME", t.TempDir())
	runCLI(t, env, dir, "init", "--with-codex-hooks")
	userPath := userCodexContractFixture(t)
	stderr := captureProcessStderr(t, func() {
		runCLI(t, env, dir, "init", "--with-codex-hooks")
	})
	assert.Contains(t, stderr, "removed workspace contract hook: "+userPath+" already injects it")
	assert.NotContains(t, stderr, "re-trust")
	assert.Equal(t, []any{map[string]any{"matcher": codexSessionStartMatcher, "hooks": []any{expectedCodexHandler()}}},
		readCodexHooks(t, dir)["hooks"].(map[string]any)["SessionStart"])

	path := filepath.Join(dir, ".codex", "hooks.json")
	first, err := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
	require.NoError(t, err)
	stderr = captureProcessStderr(t, func() {
		runCLI(t, env, dir, "init", "--with-codex-hooks")
	})
	assert.Empty(t, stderr)
	second, err := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
	require.NoError(t, err)
	assert.Equal(t, first, second)

	require.NoError(t, os.Remove(userPath))
	runCLI(t, env, dir, "init", "--with-codex-hooks")
	assert.Equal(t, expectedCodexSessionStartGroups(), readCodexHooks(t, dir)["hooks"].(map[string]any)["SessionStart"])
}

func TestE2E_InitWithCodexHooks_KeepsTrackedDuplicate(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/example-org/example-workspace.git")
	t.Setenv("CODEX_HOME", t.TempDir())
	runCLI(t, env, dir, "init", "--with-codex-hooks")
	path := filepath.Join(dir, ".codex", "hooks.json")
	before, err := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
	require.NoError(t, err)
	runGit(t, dir, "add", "--", ".codex/hooks.json")
	userPath := userCodexContractFixture(t)
	stderr := captureProcessStderr(t, func() {
		runCLI(t, env, dir, "init", "--with-codex-hooks")
	})
	assert.Contains(t, stderr, "kept workspace contract hook: .codex/hooks.json is tracked; "+userPath+" also injects it")
	after, err := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestE2E_InitWithCodexHooks_DedupeMachineOutput(t *testing.T) {
	for _, mode := range []string{"--json", "--agent"} {
		t.Run(mode, func(t *testing.T) {
			env := testenv.New(t)
			dir := t.TempDir()
			runGit(t, dir, "init", "--quiet")
			runGit(t, dir, "remote", "add", "origin", "https://github.com/example-org/example-workspace.git")
			t.Setenv("CODEX_HOME", t.TempDir())
			runCLI(t, env, dir, "init", "--with-codex-hooks")
			userCodexContractFixture(t)
			var output string
			stderr := captureProcessStderr(t, func() {
				output = runCLI(t, env, dir, "init", "--with-codex-hooks", mode)
			})
			assert.Empty(t, stderr)
			assert.Len(t, readCodexHooks(t, dir)["hooks"].(map[string]any)["SessionStart"], 1)
			if mode == "--json" {
				var parsed map[string]any
				require.NoError(t, json.Unmarshal([]byte(output), &parsed))
				assert.NotEmpty(t, parsed["project"])
			} else {
				assert.Contains(t, output, "OK init")
			}
		})
	}
}
