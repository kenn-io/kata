package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

// contractGroup returns the unconditional SessionStart group running the agent's contract hook.
func contractGroup(doc map[string]any, agent string) map[string]any {
	for _, raw := range doc["hooks"].(map[string]any)["SessionStart"].([]any) {
		group := raw.(map[string]any)
		for _, handler := range group["hooks"].([]any) {
			if _, conditional := group["if"]; !conditional && strings.Contains(handler.(map[string]any)["command"].(string), "agent-hook contract "+agent) {
				return group
			}
		}
	}
	return nil
}

func TestAgentSetupInstallsClaudeHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"agent":"claude"`)
	require.Empty(t, stderr)
	path := filepath.Join(home, "claude", "settings.json")
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Contains(t, string(data), "attention-native")
	out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.NoError(t, err)
	require.Contains(t, out, "Done.")
	require.Contains(t, stderr, "may not survive an upgrade")
	again, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, data, again)
}

func TestAgentSetupCheckDoesNotInstall(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "claude", "settings.json")
	data := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"foreign-contract-check"}]}]}}`
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.Error(t, err)
	require.Contains(t, out, "Kata hooks are not installed: kata agent setup claude")
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

// Untrusted counts are covered by doctor; a mixed result counts only modified hooks.
func TestAgentSetupCountsOnlyModifiedHooks(t *testing.T) {
	isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "normal")
	t.Setenv("TEST_HOOK_TRUST", "one-modified")
	report, err := runAgentSetup(t, "codex")
	require.Error(t, err)
	require.Equal(t, "modified", report.Rows[0].State)
	require.Equal(t, 1, report.Rows[0].HookCount)
	require.Contains(t, report.Rows[0].NextStep, "/hooks")
}

func TestAgentSetupRepairsConditionalExecutableConflict(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	data := `{"hooks":{"SessionStart":[{"if":"condition","hooks":[{"type":"command","command":"/old/kata agent-hook contract claude"}]},{"hooks":[{"type":"command","command":"/other/kata agent-hook contract claude"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	report, err := runAgentSetup(t, "claude")
	require.NoError(t, err)
	require.Equal(t, "ok", report.Rows[0].State, report.Rows[0].Reason)
	require.NotNil(t, contractGroup(readAgentHookConfig(t, path), "claude"))
	report, err = runAgentSetup(t, "claude", "--check")
	require.NoError(t, err)
	require.Equal(t, "ok", report.Rows[0].State, report.Rows[0].Reason)
}

func TestAgentSetupCodexConditionalGroups(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "normal")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	path := filepath.Join(home, "codex", "hooks.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	data := `{"hooks":{"SessionStart":[{"if":"condition","hooks":[{"type":"command","command":"/old/kata agent-hook contract codex"}]},{"hooks":[{"type":"command","command":"foreign-hook"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	trustKeys := func() string {
		cmd := exec.CommandContext(t.Context(), "codex", "app-server")
		cmd.Stdin = strings.NewReader(`{"id":1,"method":"hooks/list"}` + "\n")
		output, err := cmd.Output()
		require.NoError(t, err)
		return string(output)
	}
	foreignKey := path + ":sessionstart:1:0"
	require.Contains(t, trustKeys(), foreignKey)
	_, err := runAgentSetup(t, "codex")
	require.NoError(t, err)
	require.Contains(t, trustKeys(), foreignKey)
	doc := readAgentHookConfig(t, path)
	groups := doc["hooks"].(map[string]any)["SessionStart"].([]any)
	require.Equal(t, "foreign-hook", groups[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"])
	group := contractGroup(doc, "codex")
	require.NotNil(t, group, "contract hook must run without a condition")
	// Group metadata and a foreign handler keep the owned hook ready; a condition does not.
	group["description"] = "metadata"
	group["hooks"] = append(group["hooks"].([]any), map[string]any{"type": "command", "command": "foreign-handler"})
	writeAgentHookConfig(t, path, doc)
	report, err := runAgentSetup(t, "codex", "--check")
	require.NoError(t, err, report.Rows[0].Reason)
	group["if"] = "condition"
	writeAgentHookConfig(t, path, doc)
	report, err = runAgentSetup(t, "codex", "--check")
	require.Error(t, err)
	require.Equal(t, "outdated", report.Rows[0].State)
}

func TestAgentSetupExplicitKataOnlyAgent(t *testing.T) {
	home := isolateAgentHookHomes(t)
	report, err := runAgentSetup(t, "antigravity", "gemini", "claude")
	require.Error(t, err)
	require.Len(t, report.Rows, 3)
	for i, agent := range []string{"antigravity", "gemini"} {
		require.Equal(t, agent, report.Rows[i].Agent)
		require.Equal(t, "unknown", report.Rows[i].State)
	}
	require.Equal(t, "claude", report.Rows[2].Agent)
	require.FileExists(t, filepath.Join(home, ".gemini", "config", "hooks.json"))
	require.FileExists(t, filepath.Join(home, "gemini", ".gemini", "settings.json"))
}

func TestAgentSetupBrokenAgentDoesNotBlockOthers(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "codex", "hooks.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte("{invalid"), 0600))
	report, err := runAgentSetup(t, "claude", "codex")
	require.Error(t, err)
	require.Len(t, report.Rows, 2)
	require.Equal(t, "claude", report.Rows[0].Agent)
	require.Equal(t, "failed", report.Rows[1].State)
	require.FileExists(t, filepath.Join(home, "claude", "settings.json"))
}

func TestAgentSetupFallbackCancellation(t *testing.T) {
	home := isolateAgentHookHomes(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := agentSetupFallback(ctx, "antigravity", false, agentSetupItem("antigravity", io.Discard))
	require.ErrorIs(t, err, context.Canceled)
	require.NoFileExists(t, filepath.Join(home, ".gemini", "config", "hooks.json"))
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
