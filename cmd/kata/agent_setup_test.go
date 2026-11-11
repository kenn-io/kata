package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestAgentSetupInstallsClaudeHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	resetFlags(t)
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.NoError(t, err)
	path := filepath.Join(home, "claude", "settings.json")
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Contains(t, string(data), "attention-native")
	require.Contains(t, string(data), "contract")
	_, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.NoError(t, err)
	again, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, data, again)
}

func TestAgentSetupCheckDoesNotInstall(t *testing.T) {
	home := isolateAgentHookHomes(t)
	resetFlags(t)
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.Error(t, err)
	require.Contains(t, out, "kata agent setup claude")
	require.NoFileExists(t, filepath.Join(home, "claude", "settings.json"))
}

func TestAgentSetupCodexTrustAndDoctor(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "normal")
	t.Setenv("TEST_HOOK_TRUST", "untrusted")
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex")
	require.Error(t, err)
	require.Contains(t, out, "needs you")
	require.Contains(t, out, "/hooks")
	path := filepath.Join(home, "codex", "hooks.json")
	before, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	check := doctorFinding(t, collectDoctor(t.Context()), "agent.codex")
	require.Equal(t, "warn", string(check.Status))
	require.Contains(t, check.Summary, "3 Kata hooks wait for review")
	require.Contains(t, check.Fix, "/hooks")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex", "--check")
	require.NoError(t, err)
	require.Contains(t, out, "ok")
	check = doctorFinding(t, collectDoctor(t.Context()), "agent.codex")
	require.Equal(t, "ok", string(check.Status))
	after, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestAgentSetupOutputModes(t *testing.T) {
	isolateAgentHookHomes(t)
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"agent":"claude"`)
	out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--agent")
	require.NoError(t, err)
	require.Contains(t, out, "agent=claude")
	require.Contains(t, out, "state=ok")
}

func TestAgentSetupClaudeDisabledAndConditionalHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	data := `{"disableAllHooks":true,"hooks":{"SessionStart":[{"if":"condition","hooks":[{"type":"command","command":"kata agent-hook contract claude"},{"type":"command","command":"foreign-hook"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.Error(t, err)
	require.Contains(t, out, "disabled")
	require.Contains(t, out, "disableAllHooks")
	content, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Contains(t, string(content), "foreign-hook")
	entries, err := inspectAgentHookEntries("claude", path)
	require.NoError(t, err)
	contract, start, end := kitAgentHookConfigured("claude", entries)
	require.True(t, contract)
	require.True(t, start)
	require.True(t, end)
}

func TestAgentSetupCheckIgnoresForeignContract(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"foreign-contract-check"}]}]}}`), 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.Error(t, err)
	require.Contains(t, out, "Kata hooks are not installed")
	require.Contains(t, out, "kata agent setup claude")
}

func TestCodexConditionalHookKeepsForeignGroupIndex(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "codex", "hooks.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	data := `{"hooks":{"SessionStart":[{"if":"condition","hooks":[{"type":"command","command":"kata agent-hook contract codex","commandWindows":"kata agent-hook contract codex"}]},{"hooks":[{"type":"command","command":"foreign-hook"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	_, err := runHookSetup(t, "install", "codex", "--executable", os.Args[0])
	require.NoError(t, err)
	entries, err := inspectAgentHookEntries("codex", path)
	require.NoError(t, err)
	var foreignFound, contractFound bool
	for _, entry := range entries {
		if entry.Kind == contractHook && entry.Event == "SessionStart" {
			contractFound = true
			require.Equal(t, 0, entry.GroupIndex)
		}
		if entry.Command == "foreign-hook" {
			foreignFound = true
			require.Equal(t, 1, entry.GroupIndex)
		}
	}
	require.True(t, foreignFound)
	require.True(t, contractFound)
	contract, start, end := kitAgentHookConfigured("codex", entries)
	require.True(t, contract)
	require.True(t, start)
	require.True(t, end)
}

func TestAgentSetupCodexMissing(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Setenv("PATH", t.TempDir())
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex", "--check")
	require.Error(t, err)
	require.Contains(t, out, "Kata hooks are not installed")
	require.Contains(t, out, "kata agent setup codex")
	require.NoFileExists(t, filepath.Join(home, "codex", "hooks.json"))
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
			if name == "codex" {
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
			group["enabled"] = false
			group["if"] = "metadata"
			group["hooks"] = append(group["hooks"].([]any), map[string]any{"type": "command", "command": "foreign-hook", "custom": "preserved"})
			data, err = json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0600))
			out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name, "--check")
			require.NoError(t, err, out)
			_, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name)
			require.NoError(t, err)
			after, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
			require.NoError(t, err)
			require.Equal(t, data, after)
			entries, err := inspectAgentHookEntries(agenthook.Agent(name), path)
			require.NoError(t, err)
			contract, start, end := kitAgentHookConfigured(agenthook.Agent(name), entries)
			require.True(t, contract && start && end)
			report := agentHooksStatusJSON(t, name, "--config", path, "--workspace", t.TempDir())
			configured := report["harnesses"].([]any)[0].(map[string]any)["configured"].(map[string]any)
			require.Equal(t, true, configured["contract"])
			group["hooks"].([]any)[0].(map[string]any)["if"] = "condition"
			data, err = json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0600))
			out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name, "--check")
			require.Error(t, err)
			require.Contains(t, out, "outdated")
			_, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name)
			require.NoError(t, err)
		})
	}
}

func TestAgentSetupWarnsForUnstableExecutable(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Setenv("PATH", t.TempDir())
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--json")
	require.Error(t, err)
	require.True(t, json.Valid([]byte(out)))
	require.Contains(t, out, "Agent executable is missing")
	require.Contains(t, stderr, "may not survive an upgrade")
	require.Contains(t, stderr, "PATH")
	_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.Error(t, err)
	require.NotContains(t, stderr, "may not survive an upgrade")
}

func TestAgentSetupForeignMarker(t *testing.T) {
	home := isolateAgentHookHomes(t)
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.NoError(t, err)
	path := filepath.Join(home, "claude", "settings.json")
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	hooks := doc["hooks"].(map[string]any)
	hooks["SessionStart"] = append(hooks["SessionStart"].([]any), map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo agent-hook contract claude"}}})
	data, err = json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.NoError(t, err, out)
	require.Contains(t, out, "ok")
	groups := hooks["SessionStart"].([]any)
	owned := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
	foreign := groups[len(groups)-1].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	foreign["command"] = "echo " + owned
	data, err = json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.Error(t, err)
	require.Contains(t, out, "unknown")
}

func TestAgentSetupExplicitGemini(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Setenv("GEMINI_CLI_HOME", filepath.Join(home, "gemini"))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "gemini")
	require.Error(t, err)
	require.Contains(t, out, "unknown")
	require.FileExists(t, filepath.Join(home, "gemini", ".gemini", "settings.json"))
}

func TestAgentSetupRejectsKataOnlyHarness(t *testing.T) {
	isolateAgentHookHomes(t)
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "antigravity")
	require.ErrorContains(t, err, "kata agent-hook install antigravity")
}

func TestAgentSetupPreservesBareExecutable(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hook contract claude"}]}]}}`), 0600))
	_, _, _ = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Contains(t, string(data), "kata agent-hook contract claude")
	_, _, _ = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	again, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, data, again)
}

func TestAgentSetupConflictingExecutables(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	data := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hook contract claude"},{"type":"command","command":"/usr/bin/kata agent-hook attention-native claude start"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.Error(t, err)
	require.Contains(t, out, "unknown")
	require.Contains(t, out, "kata agent-hook install --executable")
	after, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, data, string(after))
}

func TestAgentSetupPreservesPiExecutable(t *testing.T) {
	home := isolateAgentHookHomes(t)
	_, err := runHookSetup(t, "install", "pi", "--executable", "kata")
	require.NoError(t, err)
	path := filepath.Join(home, ".pi", "agent", "extensions", "kata.js")
	before, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	_, _, _ = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "pi")
	after, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestAgentSetupCountsOnlyModifiedHooks(t *testing.T) {
	isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "normal")
	t.Setenv("TEST_HOOK_TRUST", "one-modified")
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
	require.Equal(t, "modified", report.Rows[0].State)
	require.Equal(t, 1, report.Rows[0].HookCount)
}
