package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

type agentSetupReport struct {
	Rows []struct {
		Agent     string `json:"agent"`
		State     string `json:"state"`
		Reason    string `json:"reason"`
		NextStep  string `json:"next_step"`
		HookCount int    `json:"hook_count"`
	} `json:"rows"`
}

func runAgentSetup(t *testing.T, args ...string) (agentSetupReport, error) {
	t.Helper()
	out, _, err := executeAgentHook(t, unreadableHookInput{}, append([]string{"agent", "setup", "--json"}, args...)...)
	var report agentSetupReport
	require.NoError(t, json.Unmarshal([]byte(out), &report), out)
	return report, err
}

func readAgentHookConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	return doc
}

func writeAgentHookConfig(t *testing.T, path string, doc any) {
	t.Helper()
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, data, 0600))
}

func TestAgentSetupInstallsClaudeHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"agent":"claude"`)
	require.Empty(t, stderr)
	path := filepath.Join(home, "claude", "settings.json")
	before, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.NoError(t, err)
	require.Contains(t, out, "Done.")
	require.Empty(t, stderr)
	after, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestAgentSetupKeepsLegacyOwnershipMarkers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marked map[agentHookKind]bool
	}{
		{"all hooks marked", map[agentHookKind]bool{contractHook: true, attentionStartHook: true, attentionEndHook: true}},
		{"contract only marked", map[agentHookKind]bool{contractHook: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, executableName("claude")), []byte("fixture"), 0700)) //nolint:gosec // Executable fixture in a temporary directory.
			running, err := filepath.Abs(os.Args[0])
			require.NoError(t, err)
			if err := os.Symlink(running, filepath.Join(dir, executableName("kata"))); err != nil {
				t.Skipf("stable executable alias is unavailable: %v", err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			path := filepath.Join(home, "claude", "settings.json")
			executable, _, err := resolveAgentHookExecutable("", defaultAgentHookExecutableEnv())
			require.NoError(t, err)
			opts := nativeAgentHookOptions{Agent: "claude", ConfigPath: path, Executable: executable}
			for _, kind := range []agentHookKind{contractHook, attentionStartHook, attentionEndHook} {
				if tc.marked[kind] {
					opts.OwnershipMarkers = map[agentHookKind]agentHookOwnership{kind: {Required: true}}
				} else {
					opts.OwnershipMarkers = nil
				}
				_, err := agenthook.Install(agenthook.AgentClaude, kitAgentHookRegistration(opts, kind))
				require.NoError(t, err)
			}
			before, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
			require.NoError(t, err)

			out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
			require.NoError(t, err, stderr, out)
			afterCheck, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
			require.NoError(t, err)
			require.Equal(t, before, afterCheck)

			_, err = runAgentSetup(t, "claude")
			require.NoError(t, err)
			afterSetup, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
			require.NoError(t, err)
			require.Equal(t, before, afterSetup)
		})
	}
}

func TestAgentSetupAndAgentHookInstallKeepCodexHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "trusted")
	dir := t.TempDir()
	running, err := filepath.Abs(os.Args[0])
	require.NoError(t, err)
	if err := os.Symlink(running, filepath.Join(dir, executableName("kata"))); err != nil {
		t.Skipf("stable executable alias is unavailable: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_HOOK_TRUST", "trusted")

	report, err := runAgentSetup(t, "codex")
	require.NoError(t, err, report.Rows)
	path := filepath.Join(home, "codex", "hooks.json")
	before, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under TempDir.
	require.NoError(t, err)

	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "install", "codex")
	require.NoError(t, err, stderr)
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Contains(t, out, "unchanged")
}

func TestAgentSetupCheckDoesNotInstall(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "claude", "settings.json")
	data := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"foreign-contract-check"}]}]}}`
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.Error(t, err)
	require.Contains(t, out, "missing")
	require.Contains(t, out, "kata agent setup")
	after, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, data, string(after))
}

func TestAgentSetupNoDetectedAgent(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Setenv("PATH", t.TempDir())
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "--check")
	require.ErrorContains(t, err, "no supported agent found; install Claude Code or Codex, or name an agent")
}

func TestAgentSetupUnknownAgentDoesNotInstall(t *testing.T) {
	home := isolateAgentHookHomes(t)
	report, err := runAgentSetup(t, "gemini")
	require.Error(t, err)
	require.Equal(t, "unknown", report.Rows[0].State)
	require.NoFileExists(t, filepath.Join(home, "gemini", ".gemini", "settings.json"))
	require.NoFileExists(t, filepath.Join(home, "gemini", ".gemini", "config", "hooks.json"))
}

func TestAgentSetupCodexTrustAndDeadlines(t *testing.T) {
	isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "normal")
	t.Setenv("TEST_HOOK_TRUST", "untrusted")
	report, err := runAgentSetup(t, "codex")
	require.Error(t, err)
	require.Equal(t, "untrusted", report.Rows[0].State)
	require.Equal(t, 3, report.Rows[0].HookCount)
	t.Setenv("TEST_HOOK_TRUST", "one-modified")
	status, err := checkAgentSetup(t.Context(), "codex")
	require.NoError(t, err)
	require.Equal(t, "modified", string(status.State))
	require.Equal(t, 1, status.HookCount)
	require.Contains(t, status.NextStep, "/hooks")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	t.Setenv("KATA_TEST_CODEX_AGENT", "slow")
	status, err = checkAgentSetup(t.Context(), "codex")
	require.NoError(t, err)
	require.Equal(t, "ok", string(status.State), status.Reason)
	t.Setenv("KATA_TEST_CODEX_AGENT", "hang")
	start := time.Now()
	status, err = checkAgentSetup(t.Context(), "codex")
	require.NoError(t, err)
	require.Less(t, time.Since(start), 8*time.Second)
	require.Contains(t, status.Reason, "deadline exceeded")
}

func TestAgentSetupKeepsTrustedForeignCodexHook(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "trusted")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	report, err := runAgentSetup(t, "codex")
	require.NoError(t, err, report.Rows)
	path := filepath.Join(home, "codex", "hooks.json")
	doc := readAgentHookConfig(t, path)
	hooks := doc["hooks"].(map[string]any)
	groups := hooks["SessionStart"].([]any)
	foreign := map[string]any{"matcher": "resume", "hooks": []any{map[string]any{"type": "command", "command": "foreign-hook"}}}
	hooks["SessionStart"] = append(groups, foreign)
	writeAgentHookConfig(t, path, doc)
	before, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)

	_, err = runAgentSetup(t, "codex")
	require.NoError(t, err)
	after, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterDoc := readAgentHookConfig(t, path)
	afterGroups := afterDoc["hooks"].(map[string]any)["SessionStart"].([]any)
	require.Equal(t, foreign, afterGroups[len(afterGroups)-1])
}

func TestAgentSetupPreservesCustomHookContainingSetupMarker(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	path := filepath.Join(home, "claude", "settings.json")
	foreign := map[string]any{"type": "command", "command": "kata agent-hook contract claude --source prompt.txt"}
	writeAgentHookConfig(t, path, map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{foreign}}}}})

	report, err := runAgentSetup(t, "claude")
	require.NoError(t, err, report.Rows)
	require.Equal(t, "ok", report.Rows[0].State)
	doc := readAgentHookConfig(t, path)
	groups := doc["hooks"].(map[string]any)["SessionStart"].([]any)
	var commands []any
	for _, raw := range groups {
		group := raw.(map[string]any)
		commands = append(commands, group["hooks"].([]any)...)
	}
	require.Contains(t, commands, foreign)
}

func TestAgentSetupWithBareExecutableKeepsCustomClaudeHook(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	dir := t.TempDir()
	running, err := filepath.Abs(os.Args[0])
	require.NoError(t, err)
	if err := os.Symlink(running, filepath.Join(dir, executableName("kata"))); err != nil {
		t.Skipf("stable executable alias is unavailable: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(home, "claude", "settings.json")
	foreign := map[string]any{"type": "command", "command": "kata agent-hook contract claude --source prompt.txt"}
	writeAgentHookConfig(t, path, map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{foreign}}}}})

	report, err := runAgentSetup(t, "claude")
	require.NoError(t, err, report.Rows)
	require.Equal(t, "ok", report.Rows[0].State)
	entries, err := inspectAgentHookEntries(agenthook.AgentClaude, path)
	require.NoError(t, err)
	require.Len(t, entries, 4)
	var preserved bool
	for _, entry := range entries {
		if entry.Fields["command"] == foreign["command"] {
			preserved = true
		}
	}
	require.True(t, preserved)

	before, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under TempDir.
	require.NoError(t, err)
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "install", "claude")
	require.NoError(t, err, stderr)
	require.Contains(t, out, "unchanged")
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestAgentSetupPreservesClaudeExecArgsContainingContractMarker(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	dir := t.TempDir()
	running, err := filepath.Abs(os.Args[0])
	require.NoError(t, err)
	if err := os.Symlink(running, filepath.Join(dir, executableName("kata"))); err != nil {
		t.Skipf("stable executable alias is unavailable: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(home, "claude", "settings.json")
	foreign := map[string]any{"type": "command", "command": "example-tool", "args": []any{"contract"}}
	writeAgentHookConfig(t, path, map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{foreign}}}}})

	report, err := runAgentSetup(t, "claude")
	require.NoError(t, err, report.Rows)
	require.Equal(t, "ok", report.Rows[0].State)
	entries, err := inspectAgentHookEntries(agenthook.AgentClaude, path)
	require.NoError(t, err)
	var preserved bool
	for _, entry := range entries {
		if entry.Fields["command"] == foreign["command"] {
			require.Equal(t, foreign["args"], entry.Fields["args"])
			preserved = true
		}
	}
	require.True(t, preserved)
	before, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under TempDir.
	require.NoError(t, err)
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "install", "claude")
	require.NoError(t, err, stderr)
	require.Contains(t, out, "unchanged")
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestAgentSetupPreservesCustomHookMatchingSourceMarker(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	dir := t.TempDir()
	running, err := filepath.Abs(os.Args[0])
	require.NoError(t, err)
	if err := os.Symlink(running, filepath.Join(dir, executableName("kata"))); err != nil {
		t.Skipf("stable executable alias is unavailable: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(home, "claude", "settings.json")
	foreign := map[string]any{"type": "command", "command": "env FOO=bar kata agent-hook contract claude --source kata-agent-contract-hook"}
	writeAgentHookConfig(t, path, map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{foreign}}}}})

	report, err := runAgentSetup(t, "claude")
	require.NoError(t, err, report.Rows)
	require.Equal(t, "ok", report.Rows[0].State)
	entries, err := inspectAgentHookEntries(agenthook.AgentClaude, path)
	require.NoError(t, err)
	var preserved bool
	for _, entry := range entries {
		if entry.Fields["command"] == foreign["command"] {
			preserved = true
		}
	}
	require.True(t, preserved)
	before, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under TempDir.
	require.NoError(t, err)
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "install", "claude")
	require.NoError(t, err, stderr)
	require.Contains(t, out, "unchanged")
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestAgentSetupUsesSourceMarkerWhenExecutablePathContainsContract(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "claude", "settings.json")
	dir := filepath.Join(t.TempDir(), "contracts")
	require.NoError(t, os.MkdirAll(dir, 0700))
	executable := filepath.Join(dir, executableName("kata"))
	require.NoError(t, os.WriteFile(executable, []byte("fixture"), 0700)) //nolint:gosec // Executable fixture lives under TempDir.
	workspace := t.TempDir()
	opts := nativeAgentHookOptions{Agent: "claude", ConfigPath: path, Executable: executable, Contract: true, Attention: true}
	opts.OwnershipMarkers = agentHookConfigOwnershipMarkers(agenthook.AgentClaude, path, executable)
	plan, err := planKitAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	status, err := checkAgentSetupWithOptions(t.Context(), "claude", workspace, opts, []agentHookKind{contractHook, attentionStartHook, attentionEndHook})
	require.NoError(t, err)
	require.Equal(t, "ok", string(status.State), status.Reason)
	require.Equal(t, 3, status.HookCount)
	before, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under TempDir.
	require.NoError(t, err)
	plan, err = planKitAgentHooks(opts, false)
	require.NoError(t, err)
	changed, err := publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	require.False(t, changed)
	status, err = checkAgentSetupWithOptions(t.Context(), "claude", workspace, opts, []agentHookKind{contractHook, attentionStartHook, attentionEndHook})
	require.NoError(t, err)
	require.Equal(t, "ok", string(status.State), status.Reason)
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestAgentSetupWarnsWhenCodexHookPositionsShift(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "trusted")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	report, err := runAgentSetup(t, "codex")
	require.NoError(t, err, report.Rows)

	path := filepath.Join(home, "codex", "hooks.json")
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	entries, err := parseAgentHookEntries(agenthook.AgentCodex, data)
	require.NoError(t, err)
	doc := readAgentHookConfig(t, path)
	hooks := doc["hooks"].(map[string]any)
	groups := hooks["SessionStart"].([]any)
	for _, entry := range entries {
		if entry.Kind == contractHook && entry.GroupIndex >= 0 {
			groups[entry.GroupIndex].(map[string]any)["if"] = "condition"
			break
		}
	}
	groups = append(groups, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "foreign-hook"}}})
	hooks["SessionStart"] = groups
	writeAgentHookConfig(t, path, doc)

	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex")
	require.NoError(t, err)
	require.Contains(t, stderr, "Codex will ask to re-trust shifted hooks; open Codex and run /hooks.")
}

func TestAgentSetupExplicitMixedProfiles(t *testing.T) {
	home := isolateAgentHookHomes(t)
	report, err := runAgentSetup(t, "antigravity", "gemini", "claude")
	require.Error(t, err)
	require.Len(t, report.Rows, 3)
	for i, agent := range []string{"antigravity", "gemini", "claude"} {
		require.Equal(t, agent, report.Rows[i].Agent)
	}
	require.Equal(t, "unknown", report.Rows[0].State)
	require.Equal(t, "unknown", report.Rows[1].State)
	require.FileExists(t, filepath.Join(home, "claude", "settings.json"))
	require.NoFileExists(t, filepath.Join(home, "gemini", ".gemini", "settings.json"))
}

func TestAgentSetupInvalidConfigUsesKitRecoveryGuidance(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte("{invalid"), 0600))
	report, err := runAgentSetup(t, "claude", "--check")
	require.Error(t, err)
	require.Equal(t, "unknown", report.Rows[0].State)
	require.Equal(t, "Correct the agent hook config", report.Rows[0].NextStep)
	require.NoError(t, os.Remove(path))
	retry, err := runAgentSetup(t, "claude")
	require.NoError(t, err)
	require.Len(t, retry.Rows, 1)
	require.Equal(t, "claude", retry.Rows[0].Agent)
}

func installClaudeAgentFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, executableName("claude")), []byte("fixture"), 0700)) //nolint:gosec // Executable fixture in a temporary directory.
	t.Setenv("PATH", dir)
}

// installCodexAgentFixture builds a fake Codex app-server; Kit's fixture is internal to Kit.
func installCodexAgentFixture(t *testing.T, mode string) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(dir, executableName("codex")), "./testdata/codex-agent") //nolint:gosec // Arguments and destination are test controlled.
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KATA_TEST_CODEX_AGENT", mode)
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
