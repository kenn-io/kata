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
	_, _ = runAgentSetup(t, "codex")
	check := doctorAgent(t.Context(), "codex")
	require.Equal(t, "warn", string(check.Status))
	require.Contains(t, check.Summary, "3 Kata hooks wait for review")
	require.Contains(t, check.Fix, "/hooks")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	check = doctorAgent(t.Context(), "codex")
	require.Equal(t, "ok", string(check.Status), check.Summary)
	// A hung start exceeds its five-second bound.
	t.Setenv("KATA_TEST_CODEX_AGENT", "hang")
	start := time.Now()
	check = doctorAgent(t.Context(), "codex")
	require.Less(t, time.Since(start), 8*time.Second)
	require.Contains(t, check.Summary, "deadline exceeded")
}

func TestDoctorClaudeHooksAndAbsentCodex(t *testing.T) {
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
	require.Contains(t, check.Summary, "Kata hooks need updating")
	require.Equal(t, "info", string(doctorAgent(t.Context(), "codex").Status))
}
