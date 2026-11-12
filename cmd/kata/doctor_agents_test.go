package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDoctorAgentProbeDeadline(t *testing.T) {
	isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "hang")
	opts, _, err := agentSetupOptions("codex")
	require.NoError(t, err)
	plan, err := planNativeAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	start := time.Now()
	check := doctorAgent(t.Context(), "codex")
	require.Less(t, time.Since(start), 8*time.Second)
	require.Contains(t, check.Summary, "deadline exceeded")
	installCodexAgentFixture(t, "normal")
	t.Setenv("TEST_HOOK_TRUST", "untrusted")
	check = doctorAgent(t.Context(), "codex")
	require.Equal(t, "warn", string(check.Status))
	require.Contains(t, check.Summary, "3 Kata hooks wait for review")
	require.Contains(t, check.Fix, "/hooks")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	check = doctorAgent(t.Context(), "codex")
	require.Equal(t, "ok", string(check.Status))
}

func TestDoctorClaudeDisabledHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	_, err := runHookSetup(t, "install", "claude", "--executable", os.Args[0])
	require.NoError(t, err)
	path := filepath.Join(home, "claude", "settings.json")
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	doc["disableAllHooks"] = true
	data, err = json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	check := doctorAgent(t.Context(), "claude")
	require.Equal(t, "warn", string(check.Status))
	require.Contains(t, check.Fix, "disableAllHooks")
	codex := doctorAgent(t.Context(), "codex")
	require.Equal(t, "info", string(codex.Status))
}

func TestDoctorAgentContractOnly(t *testing.T) {
	isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	opts, _, err := agentSetupOptions("claude")
	require.NoError(t, err)
	_, err = runHookSetup(t, "install", "claude", "--executable", opts.Executable, "--contract-only")
	require.NoError(t, err)
	check := doctorAgent(t.Context(), "claude")
	require.Equal(t, "ok", string(check.Status), check.Summary)
}

func TestDoctorAgentTotalDeadline(t *testing.T) {
	isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "normal")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex")
	require.NoError(t, err)
	t.Setenv("KATA_TEST_CODEX_AGENT", "slower")
	start := time.Now()
	check := doctorAgent(t.Context(), "codex")
	require.Less(t, time.Since(start), 12*time.Second)
	require.Equal(t, "warn", string(check.Status))
	require.Contains(t, check.Summary, "deadline exceeded")
}

func TestDoctorAgentConditionalAttention(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.NoError(t, err)
	path := filepath.Join(home, "claude", "settings.json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	hooks := doc["hooks"].(map[string]any)
	for _, raw := range hooks {
		for _, item := range raw.([]any) {
			group := item.(map[string]any)
			for _, handler := range group["hooks"].([]any) {
				if strings.Contains(handler.(map[string]any)["command"].(string), "attention-native") {
					group["if"] = "condition"
				}
			}
		}
	}
	data, err = json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	check := doctorAgent(t.Context(), "claude")
	require.Equal(t, "warn", string(check.Status), check.Summary)
}
