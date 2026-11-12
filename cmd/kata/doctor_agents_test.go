package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDoctorAgentCodexTrust(t *testing.T) {
	isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "normal")
	t.Setenv("TEST_HOOK_TRUST", "untrusted")
	_, err := runAgentSetup(t, "codex")
	require.Error(t, err)
	check := doctorAgent(t.Context(), "codex")
	require.Equal(t, "warn", string(check.Status))
	require.Contains(t, check.Summary, "3 Kata hooks wait for review")
	require.Contains(t, check.Fix, "/hooks")
}

func TestDoctorClaudeHooksAndAbsentCodex(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	_, err := runAgentSetup(t, "claude")
	require.NoError(t, err)
	path := filepath.Join(home, "claude", "settings.json")
	t.Run("disabled", func(t *testing.T) {
		doc := readAgentHookConfig(t, path)
		doc["disableAllHooks"] = true
		writeAgentHookConfig(t, path, doc)
		check := doctorAgent(t.Context(), "claude")
		require.Equal(t, "warn", string(check.Status))
		require.Contains(t, check.Fix, "disableAllHooks")
		delete(doc, "disableAllHooks")
		writeAgentHookConfig(t, path, doc)
	})
	t.Run("outdated", func(t *testing.T) {
		doc := readAgentHookConfig(t, path)
		delete(doc["hooks"].(map[string]any), "SessionEnd")
		writeAgentHookConfig(t, path, doc)
		check := doctorAgent(t.Context(), "claude")
		require.Equal(t, "warn", string(check.Status), check.Summary)
		require.Contains(t, check.Summary, "Kata hooks need updating")
	})
	t.Run("absent codex", func(t *testing.T) {
		require.Equal(t, "info", string(doctorAgent(t.Context(), "codex").Status))
	})
}

func TestDoctorAgentContractOnly(t *testing.T) {
	isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	status, err := checkAgentSetup(t.Context(), "claude")
	require.NoError(t, err)
	require.Equal(t, "missing", string(status.State))
	opts, err := agentSetupOptions("claude")
	require.NoError(t, err)
	_, err = runHookSetup(t, "install", "claude", "--executable", opts.Executable, "--contract-only")
	require.NoError(t, err)
	check := doctorAgent(t.Context(), "claude")
	require.Equal(t, "ok", string(check.Status), check.Summary)
}
