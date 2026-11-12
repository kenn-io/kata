package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentSetupInstallsClaudeHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	resetFlags(t)
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"agent":"claude"`)
	path := filepath.Join(home, "claude", "settings.json")
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Contains(t, string(data), "attention-native")
	require.Contains(t, string(data), "contract")
	out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--agent")
	require.NoError(t, err)
	require.Contains(t, out, "agent=claude")
	require.Contains(t, out, "state=ok")
	again, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, data, again)
}

func TestAgentSetupCheckDoesNotInstall(t *testing.T) {
	home := isolateAgentHookHomes(t)
	resetFlags(t)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	data := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"foreign-contract-check"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.Error(t, err)
	require.Contains(t, out, "Kata hooks are not installed")
	require.Contains(t, out, "kata agent setup claude")
	after, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, data, string(after))
}

func TestAgentSetupCodexTrust(t *testing.T) {
	isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "normal")
	t.Setenv("TEST_HOOK_TRUST", "untrusted")
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex")
	require.Error(t, err)
	require.Contains(t, out, "needs you")
	require.Contains(t, out, "/hooks")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex", "--check", "--agent")
	require.NoError(t, err)
	require.Contains(t, out, "state=ok")
}

func TestAgentSetupNoDetectedAgent(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Setenv("PATH", t.TempDir())
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "--check")
	require.ErrorContains(t, err, "no supported agent found; install Claude Code or Codex, or name an agent")
}

func TestAgentSetupPreservesGroupMetadata(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			if name == "claude" {
				installClaudeAgentFixture(t)
			} else {
				installCodexAgentFixture(t, "normal")
				t.Setenv("TEST_HOOK_TRUST", "trusted")
			}
			_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name)
			require.NoError(t, err)
			file := "settings.json"
			if name == "codex" {
				file = "hooks.json"
			}
			path := filepath.Join(home, name, file)
			data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
			require.NoError(t, err)
			var doc map[string]any
			require.NoError(t, json.Unmarshal(data, &doc))
			group := doc["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)
			group["description"] = "metadata"
			group["hooks"] = append(group["hooks"].([]any), map[string]any{"type": "command", "command": "foreign-hook", "custom": "preserved"})
			data, err = json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0600))
			out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name, "--check")
			require.NoError(t, err, out)
			if name == "codex" {
				group["if"] = "condition"
				data, err = json.Marshal(doc)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0600))
				out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name, "--check")
				require.Error(t, err)
				require.Contains(t, out, "outdated")
			}
		})
	}
}

func TestAgentSetupWarnsForUnstableExecutable(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Setenv("PATH", t.TempDir())
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--json")
	require.NoError(t, err)
	require.True(t, json.Valid([]byte(out)))
	require.Empty(t, stderr)
	_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.NoError(t, err)
	require.Contains(t, stderr, "may not survive an upgrade")
	_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.NoError(t, err)
	require.NotContains(t, stderr, "may not survive an upgrade")
}

func TestAgentSetupForeignMarker(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.NoError(t, err)
	path := filepath.Join(home, "claude", "settings.json")
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	hooks := doc["hooks"].(map[string]any)
	groups := hooks["SessionStart"].([]any)
	owned := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
	hooks["SessionStart"] = append(groups, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo " + owned}}})
	data, err = json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.Error(t, err)
	require.Contains(t, out, "unknown")
}

func TestAgentSetupRepairsConditionalExecutableConflict(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("recovery command uses a POSIX shell")
	}
	bin := buildAgentSetupRecovery(t)
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	data := `{"hooks":{"SessionStart":[{"if":"condition","hooks":[{"type":"command","command":"/old/kata agent-hook contract claude"}]},{"hooks":[{"type":"command","command":"/other/kata agent-hook contract claude"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	run := func(args ...string) []byte {
		cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // Test-built executable and fixed arguments.
		output, _ := cmd.Output()
		return output
	}
	var report struct {
		Rows []struct {
			State    string `json:"state"`
			Reason   string `json:"reason"`
			NextStep string `json:"next_step"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(run("agent", "setup", "claude", "--json"), &report))
	require.Len(t, report.Rows, 1)
	if report.Rows[0].Reason == "kata hooks use different executables" {
		repair := exec.CommandContext(t.Context(), "/bin/sh", "-c", report.Rows[0].NextStep) //nolint:gosec // Execute the reported recovery in an isolated home.
		output, err := repair.CombinedOutput()
		require.NoError(t, err, string(output))
	}
	report.Rows = nil
	require.NoError(t, json.Unmarshal(run("agent", "setup", "claude", "--check", "--json"), &report))
	require.Len(t, report.Rows, 1)
	require.Equal(t, "ok", report.Rows[0].State, report.Rows[0].Reason)
}

func TestAgentSetupCountsOnlyModifiedHooks(t *testing.T) {
	for _, tc := range []struct {
		trust, state string
		count        int
	}{{"untrusted", "untrusted", 3}, {"one-modified", "modified", 1}} {
		t.Run(tc.trust, func(t *testing.T) {
			isolateAgentHookHomes(t)
			installCodexAgentFixture(t, "normal")
			t.Setenv("TEST_HOOK_TRUST", tc.trust)
			out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex", "--json")
			require.Error(t, err)
			var report struct {
				Rows []struct {
					State     string `json:"state"`
					HookCount int    `json:"hook_count"`
				} `json:"rows"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &report))
			require.Len(t, report.Rows, 1)
			require.Equal(t, tc.state, report.Rows[0].State)
			require.Equal(t, tc.count, report.Rows[0].HookCount)
		})
	}
}

func TestAgentSetupBrokenAgentDoesNotBlockOthers(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "codex", "hooks.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte("{invalid"), 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "codex", "--json")
	require.Error(t, err)
	var report struct {
		Rows []struct {
			Agent string `json:"agent"`
			State string `json:"state"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Rows, 2)
	require.Equal(t, "claude", report.Rows[0].Agent)
	require.Equal(t, "failed", report.Rows[1].State)
	require.FileExists(t, filepath.Join(home, "claude", "settings.json"))
}

func TestAgentSetupExplicitKataOnlyAgent(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Setenv("GEMINI_CLI_HOME", filepath.Join(home, "gemini"))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "antigravity", "--check", "--json")
	require.Error(t, err)
	require.Contains(t, out, `"state":"missing"`)
	require.NoFileExists(t, filepath.Join(home, ".gemini", "config", "hooks.json"))
	out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "antigravity", "gemini", "claude", "--json")
	require.Error(t, err)
	var report struct {
		Rows []struct {
			Agent string `json:"agent"`
			State string `json:"state"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Rows, 3)
	require.Equal(t, "antigravity", report.Rows[0].Agent)
	require.Equal(t, "unknown", report.Rows[0].State, out)
	require.Equal(t, "gemini", report.Rows[1].Agent)
	require.Equal(t, "unknown", report.Rows[1].State, out)
	require.Equal(t, "claude", report.Rows[2].Agent)
	require.FileExists(t, filepath.Join(home, ".gemini", "config", "hooks.json"))
	require.FileExists(t, filepath.Join(home, "gemini", ".gemini", "settings.json"))
}

func TestAgentSetupKataOnlyProfiles(t *testing.T) {
	for _, name := range []string{"antigravity", "amp", "openclaw", "kimi-code", "kimi", "grok", "zcode", "pi"} {
		t.Run(name, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name, "--json")
			require.Error(t, err)
			require.Contains(t, out, `"state":"unknown"`)
			path, err := agentHookScopePath(name, "user", home)
			require.NoError(t, err)
			_, err = os.Stat(path)
			require.NoError(t, err)
		})
	}
}

func TestAgentSetupFallbackCancellation(t *testing.T) {
	home := isolateAgentHookHomes(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := agentSetupFallback(ctx, "antigravity", false, agentSetupItem("antigravity", func(string) {}, io.Discard))
	require.ErrorIs(t, err, context.Canceled)
	require.NoFileExists(t, filepath.Join(home, ".gemini", "config", "hooks.json"))
}

func installClaudeAgentFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0700)) //nolint:gosec // Executable fixture in a temporary directory.
	t.Setenv("PATH", dir)
}

func buildAgentSetupRecovery(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kata with spaces")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".") //nolint:gosec // Test-owned output and fixed package.
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	return bin
}

func TestAgentSetupPreservesCodexForeignGroupIndex(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "normal")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	path := filepath.Join(home, "codex", "hooks.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	data := `{"hooks":{"SessionStart":[{"if":"condition","hooks":[{"type":"command","command":"/old/kata agent-hook contract codex"}]},{"hooks":[{"type":"command","command":"foreign-hook"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	_, _, _ = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex")
	dataBytes, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(dataBytes, &doc))
	groups := doc["hooks"].(map[string]any)["SessionStart"].([]any)
	foreign := groups[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	require.Equal(t, "foreign-hook", foreign["command"])
}

func TestAgentSetupSlowCodexChecks(t *testing.T) {
	isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "slow")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex")
	require.NoError(t, err)
	check := doctorAgent(t.Context(), "codex")
	require.Equal(t, "ok", string(check.Status), check.Summary)
}
