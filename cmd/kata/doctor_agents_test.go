package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestDoctorAgentMissingWithHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Setenv("PATH", t.TempDir())
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hook contract claude"}]}]}}`), 0600))
	check := doctorAgent(t.Context(), "claude")
	require.Equal(t, "warn", string(check.Status))
	require.Contains(t, check.Summary, "unknown")
	require.Contains(t, check.Fix, "PATH")
}
