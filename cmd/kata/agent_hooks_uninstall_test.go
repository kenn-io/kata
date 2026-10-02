package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestAgentHooksUninstallAllProfiles(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	for _, profile := range agenthook.Profiles() {
		if profile.Agent == agenthook.AgentDroid {
			continue
		}
		t.Run(string(profile.Agent), func(t *testing.T) {
			path, err := agenthook.ConfigPath(profile.Agent)
			require.NoError(t, err)
			hook := agenthook.Hook{Event: agenthook.EventSessionStart}
			if profile.Agent == agenthook.AgentHermes {
				hook.Event = agenthook.EventUserPromptSubmit
			}
			installHookFixture(t, profile.Agent, path, "before", "example-before", hook)
			installHookFixture(t, profile.Agent, path, "kata", legacyAgentContractHookSource, hook)
			installHookFixture(t, profile.Agent, path, "attention", legacyAttentionHookSource+"start", hook)
			installHookFixture(t, profile.Agent, path, "after", "example-after", hook)
			out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "uninstall", string(profile.Agent))
			require.NoError(t, err, stderr)
			require.Contains(t, out, "removed")
			if profile.Agent == agenthook.AgentCodex {
				require.Contains(t, out, "init --with-codex-hooks")
				require.NotContains(t, out, "Codex runs new hooks")
			}
			handlers := nativeContractHandlers(t, profile.Agent, path)
			require.Len(t, handlers, 3)
			for i, marker := range []string{"example-before", legacyAttentionHookSource + "start", "example-after"} {
				command, _ := handlers[i]["command"].(string)
				if command == "" {
					command, _ = handlers[i]["bash"].(string)
				}
				require.Contains(t, command, marker)
			}
			before, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook config fixture under TempDir.
			require.NoError(t, err)
			out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "uninstall", string(profile.Agent), "--json")
			require.NoError(t, err, stderr)
			var report struct {
				Results []agentHookMutation `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &report))
			require.False(t, report.Results[0].Changed)
			require.Equal(t, "absent", report.Results[0].State)
			after, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook config fixture under TempDir.
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestAgentHooksUninstallMissingAndOverride(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	config := filepath.Join(t.TempDir(), "second", "hooks.json")
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "uninstall", "codex", "--config", config)
	require.NoError(t, err, stderr)
	require.Contains(t, out, "absent")
	_, err = os.Stat(filepath.Dir(config))
	require.ErrorIs(t, err, os.ErrNotExist)
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Empty(t, entries)
	installHookFixture(t, agenthook.AgentCodex, config, "kata", legacyAgentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "uninstall", "codex", "--config", config)
	require.NoError(t, err, stderr)
	entries, err = os.ReadDir(home)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestAgentHooksUninstallPreflightAndModes(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	claude, err := agenthook.ConfigPath(agenthook.AgentClaude)
	require.NoError(t, err)
	codex, err := agenthook.ConfigPath(agenthook.AgentCodex)
	require.NoError(t, err)
	installHookFixture(t, agenthook.AgentClaude, claude, "kata", legacyAgentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	before, err := os.ReadFile(claude) //nolint:gosec // G304: isolated hook config fixture under TempDir.
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(codex), 0o700))
	require.NoError(t, os.WriteFile(codex, []byte("{broken"), 0o600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "uninstall", "claude", "codex", "--json")
	require.Error(t, err)
	require.Empty(t, out)
	after, err := os.ReadFile(claude) //nolint:gosec // G304: isolated hook config fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "uninstall", "claude", "--agent")
	require.NoError(t, err, stderr)
	require.Contains(t, out, "state=removed")
	require.Contains(t, out, "changed=true")
	out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "uninstall", "claude", "--quiet")
	require.NoError(t, err, stderr)
	require.Empty(t, out)
	out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "uninstall", "claude", "--quiet", "--json")
	require.NoError(t, err, stderr)
	require.Contains(t, out, `"kata_api_version":1`)
	for _, args := range [][]string{{}, {"claude", "--all"}, {"claude", "unknown"}, {"--all", "--config", "example"}, {"codex", "--config="}} {
		out, stderr, err = executeAgentHook(t, unreadableHookInput{}, append([]string{"agent-hook", "uninstall"}, args...)...)
		require.Error(t, err)
		require.Empty(t, out)
		require.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered), stderr)
	}
}
