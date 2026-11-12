package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDoctorAgentCodexTrustAndDeadlines(t *testing.T) {
	isolateAgentHookHomes(t)
	// Each of the three checks starts Codex in two seconds; together they fit the row bound.
	installCodexAgentFixture(t, "slow")
	t.Setenv("TEST_HOOK_TRUST", "untrusted")
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex")
	require.Error(t, err)
	require.Equal(t, 1, strings.Count(stderr, "Native SessionEnd requires a current Codex runtime"))
	check := doctorAgent(t.Context(), "codex")
	require.Equal(t, "warn", string(check.Status))
	require.Contains(t, check.Summary, "3 Kata hooks wait for review")
	require.Contains(t, check.Fix, "/hooks")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	check = doctorAgent(t.Context(), "codex")
	require.Equal(t, "ok", string(check.Status), check.Summary)
	// Three four-second starts exceed the ten-second row bound; one hung start exceeds its five-second bound.
	for mode, limit := range map[string]time.Duration{"slower": 12 * time.Second, "hang": 8 * time.Second} {
		t.Setenv("KATA_TEST_CODEX_AGENT", mode)
		start := time.Now()
		check = doctorAgent(t.Context(), "codex")
		require.Less(t, time.Since(start), limit, mode)
		require.Contains(t, check.Summary, "deadline exceeded", mode)
	}
}

func TestDoctorAgentContractOnly(t *testing.T) {
	isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	opts, err := agentSetupOptions("claude")
	require.NoError(t, err)
	_, err = runHookSetup(t, "install", "claude", "--executable", opts.Executable, "--contract-only")
	require.NoError(t, err)
	check := doctorAgent(t.Context(), "claude")
	require.Equal(t, "ok", string(check.Status), check.Summary)
}

func TestDoctorClaudeConditionalAndDisabledHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	_, err := runAgentSetup(t, "claude")
	require.NoError(t, err)
	path := filepath.Join(home, "claude", "settings.json")
	doc := readAgentHookConfig(t, path)
	doc["disableAllHooks"] = true
	writeAgentHookConfig(t, path, doc)
	check := doctorAgent(t.Context(), "claude")
	require.Equal(t, "warn", string(check.Status))
	require.Contains(t, check.Fix, "disableAllHooks")
	delete(doc, "disableAllHooks")
	for _, groups := range doc["hooks"].(map[string]any) {
		for _, raw := range groups.([]any) {
			group := raw.(map[string]any)
			if strings.Contains(group["hooks"].([]any)[0].(map[string]any)["command"].(string), "attention-native") {
				group["if"] = "condition"
			}
		}
	}
	writeAgentHookConfig(t, path, doc)
	check = doctorAgent(t.Context(), "claude")
	require.Equal(t, "warn", string(check.Status), check.Summary)
	require.Equal(t, "info", string(doctorAgent(t.Context(), "codex").Status))
}
