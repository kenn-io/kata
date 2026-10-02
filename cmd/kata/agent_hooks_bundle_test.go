package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestKitAgentHookAttentionBundle(t *testing.T) {
	for _, target := range []string{"claude", "codex", "gemini", "copilot", "cursor", "qwen", "hermes"} {
		t.Run(target, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			opts := nativeAgentHookOptions{Agent: target, Scope: "user", ConfigPath: path, Executable: "kata", Contract: true, Attention: true}
			plan, err := planKitAgentHooks(opts, false)
			require.NoError(t, err)
			require.True(t, plan.Contract)
			require.True(t, plan.AttentionStart)
			require.True(t, plan.AttentionEnd)
			_, err = publishNativeAgentHookPlan(plan)
			require.NoError(t, err)
			entries, err := inspectAgentHookEntries(agenthook.Agent(target), path)
			require.NoError(t, err)
			kinds := map[agentHookKind]int{}
			for _, entry := range entries {
				if entry.Kind != "" {
					kinds[entry.Kind]++
				}
				if target == "hermes" && entry.Kind == attentionEndHook {
					require.Equal(t, "on_session_finalize", entry.Event)
				}
			}
			startCount := 1
			if target == "hermes" {
				startCount = 2
			}
			require.Equal(t, map[agentHookKind]int{contractHook: 1, attentionStartHook: startCount, attentionEndHook: 1}, kinds)
			// Repair/reinstall is byte stable and default contract operations leave the
			// independently installed lifecycle directions present.
			before, err := os.ReadFile(path) //nolint:gosec // G304: isolated native configuration fixture under TempDir.
			require.NoError(t, err)
			opts.Attention = false
			again, err := planKitAgentHooks(opts, false)
			require.NoError(t, err)
			changed, err := publishNativeAgentHookPlan(again)
			require.NoError(t, err)
			require.False(t, changed)
			after, err := os.ReadFile(path) //nolint:gosec // G304: isolated native configuration fixture under TempDir.
			require.NoError(t, err)
			require.Equal(t, before, after)
			removal, err := planKitAgentHooks(opts, true)
			require.NoError(t, err)
			_, err = publishNativeAgentHookPlan(removal)
			require.NoError(t, err)
			entries, err = inspectAgentHookEntries(agenthook.Agent(target), path)
			require.NoError(t, err)
			require.Len(t, entries, startCount+1)
			opts.Attention = true
			removal, err = planKitAgentHooks(opts, true)
			require.NoError(t, err)
			_, err = publishNativeAgentHookPlan(removal)
			require.NoError(t, err)
			entries, err = inspectAgentHookEntries(agenthook.Agent(target), path)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestKitAgentHookBundlePreservesForeignHandlers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	foreign := []byte(`{"theme":"authored","hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"echo authored-before"},{"command":"kata agent-hooks attention-native claude start && echo custom"}]}],"SessionEnd":[{"hooks":[{"command":"echo authored-end"}]}]}}`)
	require.NoError(t, os.WriteFile(path, foreign, 0600))
	plan, err := planKitAgentHooks(nativeAgentHookOptions{Agent: "claude", ConfigPath: path, Executable: "kata", Contract: true, Attention: true}, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	entries, err := inspectAgentHookEntries(agenthook.AgentClaude, path)
	require.NoError(t, err)
	var commands []string
	for _, entry := range entries {
		if entry.Kind == "" {
			commands = append(commands, entry.Command)
		}
	}
	require.Equal(t, []string{"echo authored-end", "echo authored-before", "kata agent-hooks attention-native claude start && echo custom"}, commands)
}

func TestKitAgentHookHermesReplacementStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	plan, err := planKitAgentHooks(nativeAgentHookOptions{Agent: "hermes", ConfigPath: path, Executable: "kata", Attention: true}, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	entries, err := inspectAgentHookEntries(agenthook.AgentHermes, path)
	require.NoError(t, err)
	var events []string
	for _, entry := range entries {
		if entry.Kind == attentionStartHook {
			events = append(events, entry.Event)
		}
	}
	require.ElementsMatch(t, []string{"on_session_start", "on_session_reset"}, events)
}

func TestKitAgentHookConfiguredRejectsConditionalHandlers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"command":"kata agent-hooks contract claude","async":true}]},{"enabled":false,"hooks":[{"command":"kata agent-hooks attention-native claude start"}]}]}}`), 0600))
	plan, err := planKitAgentHooks(nativeAgentHookOptions{Agent: "claude", ConfigPath: path}, true)
	require.NoError(t, err)
	require.False(t, plan.CurrentContract)
	require.False(t, plan.CurrentAttentionStart)
	plan, err = planKitAgentHooks(nativeAgentHookOptions{Agent: "claude", ConfigPath: path, Executable: "kata", Contract: true, Attention: true}, false)
	require.NoError(t, err)
	require.True(t, plan.Contract)
	require.True(t, plan.AttentionStart)
}

func TestKitAgentHookConfiguredRejectsRestrictiveMatchers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"hooks":{"SessionStart":[{"matcher":"^never$","hooks":[{"command":"kata agent-hooks contract claude"},{"command":"kata agent-hooks attention-native claude start"}]}]}}`), 0600))
	plan, err := planKitAgentHooks(nativeAgentHookOptions{Agent: "claude", ConfigPath: path}, true)
	require.NoError(t, err)
	require.False(t, plan.CurrentContract)
	require.False(t, plan.CurrentAttentionStart)
	plan, err = planKitAgentHooks(nativeAgentHookOptions{Agent: "claude", ConfigPath: path, Executable: "kata", Contract: true, Attention: true}, false)
	require.NoError(t, err)
	require.True(t, plan.Contract)
	require.True(t, plan.AttentionStart)
}

func TestKitAgentHookAttentionMatchesExactSource(t *testing.T) {
	for _, target := range []string{"gemini", "qwen"} {
		t.Run(target, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			plan, err := planKitAgentHooks(nativeAgentHookOptions{Agent: target, ConfigPath: path, Executable: "kata", Attention: true}, false)
			require.NoError(t, err)
			entries, err := parseAgentHookEntries(agenthook.Agent(target), plan.Changes[0].Content)
			require.NoError(t, err)
			var sources []string
			for _, source := range []string{"startup", "resume", "clear"} {
				for _, entry := range entries {
					if entry.Kind == attentionStartHook && (entry.Matcher == "" || entry.Matcher == source) {
						sources = append(sources, source)
					}
				}
			}
			require.Equal(t, []string{"startup", "resume", "clear"}, sources)
		})
	}
}
