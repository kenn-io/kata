package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/diagnostics"
)

func TestDoctorSelectedWorkspace(t *testing.T) {
	isolateAgentHookHomes(t)
	_, workspace := doctorTestEnv(t)
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1")
	binary := filepath.Join(t.TempDir(), executableName("kata"))
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	installCodexAgentFixture(t, "normal")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, executableName("claude")), []byte("fixture"), 0700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = runHookSetup(t, "install", "claude", "--executable", binary)
	require.NoError(t, err)
	_, err = runHookSetup(t, "install", "codex", "--executable", binary)
	require.NoError(t, err)
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	t.Setenv("KATA_TEST_EXPECT_WORKSPACE", workspace)
	writeAgentHookConfig(t, filepath.Join(workspace, ".claude", "settings.local.json"), map[string]any{"disableAllHooks": true})
	cmd := exec.CommandContext(t.Context(), binary, "doctor", "--workspace", workspace, "--json")
	output, _ := cmd.Output()
	var report diagnostics.Report
	require.NoError(t, json.Unmarshal(output, &report), string(output))
	seen := map[string]bool{}
	for _, check := range report.Checks {
		if check.ID == "agent.claude" {
			require.Equal(t, "warn", string(check.Status), check.Summary)
			require.Contains(t, check.Fix, "disableAllHooks")
			seen[check.ID] = true
		}
		if check.ID == "agent.codex" {
			require.Equal(t, "ok", string(check.Status), check.Summary)
			seen[check.ID] = true
		}
	}
	require.Len(t, seen, 2)
}

func TestDoctorBareCodexExecutable(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "normal")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, executableName("kata")), []byte("fixture"), 0700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := runHookSetup(t, "install", "codex", "--executable", "kata")
	require.NoError(t, err)
	path := filepath.Join(home, "codex", "hooks.json")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	t.Setenv("TEST_HOOK_TRUST", "untrusted")
	check := doctorAgent(t.Context(), "codex")
	require.Equal(t, "warn", string(check.Status), check.Summary)
	require.Contains(t, check.Fix, "/hooks")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

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

func TestDoctorAgentRepairsIneffectiveAttention(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	_, err := runAgentSetup(t, "claude")
	require.NoError(t, err)
	path := filepath.Join(home, "claude", "settings.json")
	doc := readAgentHookConfig(t, path)
	for _, event := range []string{"SessionStart", "SessionEnd"} {
		for _, group := range doc["hooks"].(map[string]any)[event].([]any) {
			if strings.Contains(fmt.Sprint(group), "attention-native") {
				group.(map[string]any)["matcher"] = "never-matches"
			}
		}
	}
	writeAgentHookConfig(t, path, doc)
	check := doctorAgent(t.Context(), "claude")
	require.Equal(t, "warn", string(check.Status), check.Summary)
	require.Equal(t, "kata agent setup claude", check.Fix)
	_, err = runAgentSetup(t, "claude")
	require.NoError(t, err)
	check = doctorAgent(t.Context(), "claude")
	require.Equal(t, "ok", string(check.Status), check.Summary)
}

func TestDoctorAgentRemovedAfterSetup(t *testing.T) {
	isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	_, err := runAgentSetup(t, "claude")
	require.NoError(t, err)
	t.Setenv("PATH", t.TempDir())
	check := doctorAgent(t.Context(), "claude")
	require.Equal(t, "info", string(check.Status), check.Summary)
	require.Contains(t, check.Summary, "not installed")
}
