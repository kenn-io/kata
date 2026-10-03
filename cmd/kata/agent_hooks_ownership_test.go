package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestAgentHooksUninstallPreservesUnmarkedExecutableWrappers(t *testing.T) {
	for _, target := range []string{"claude", "droid", "zcode"} {
		t.Run(target, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			before := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"command":"/opt/wrappers/notify agent-hook contract claude"},{"command":"/opt/wrappers/notify agent-hook attention start"}]}]}}`)
			switch target {
			case "droid":
				before = []byte(`{"SessionStart":[{"hooks":[{"command":"/opt/wrappers/notify agent-hook contract droid"},{"command":"/opt/wrappers/notify agent-hook attention-native droid start"}]}]}`)
			case "zcode":
				before = []byte(`{"hooks":{"events":{"SessionStart":[{"hooks":[{"type":"process","command":"C:\\wrappers\\notify.exe","args":["agent-hook","contract","zcode"]}]}]}}}`)
			}
			require.NoError(t, os.WriteFile(path, before, 0o600))
			opts := nativeAgentHookOptions{Agent: target, ConfigPath: path, Contract: true, Attention: true}
			var plan nativeAgentHookPlan
			var err error
			if target == "claude" {
				plan, err = planKitAgentHooks(opts, true)
			} else {
				opts.Home = t.TempDir()
				plan, err = planExtraAgentHooks(opts, true)
			}
			require.NoError(t, err)
			changed, err := publishNativeAgentHookPlan(plan)
			require.NoError(t, err)
			require.False(t, changed)
			after, err := os.ReadFile(path) //nolint:gosec // G304: temporary authored-hook fixture.
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestAgentHooksRenamedExecutableInstallAndUninstall(t *testing.T) {
	for _, target := range []string{"claude", "droid", "zcode"} {
		t.Run(target, func(t *testing.T) {
			opts := nativeAgentHookOptions{Agent: target, Home: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "hooks.json"), Executable: "/opt/example/custom-launcher", Contract: true, Attention: true}
			planHooks := planExtraAgentHooks
			if target == "claude" {
				planHooks = planKitAgentHooks
			}
			plan, err := planHooks(opts, false)
			require.NoError(t, err)
			_, err = publishNativeAgentHookPlan(plan)
			require.NoError(t, err)
			if target == "claude" {
				entries, err := inspectAgentHookEntries(agenthook.AgentClaude, opts.ConfigPath)
				require.NoError(t, err)
				require.Len(t, entries, 3)
				for _, entry := range entries {
					require.Contains(t, entry.Command, "--source kata-agent-")
				}
			}
			again, err := planHooks(opts, false)
			require.NoError(t, err)
			require.True(t, again.CurrentContract)
			changed, err := publishNativeAgentHookPlan(again)
			require.NoError(t, err)
			require.False(t, changed)
			removed, err := planHooks(opts, true)
			require.NoError(t, err)
			require.False(t, removed.Contract)
			require.False(t, removed.AttentionStart)
			require.False(t, removed.AttentionEnd)
			changed, err = publishNativeAgentHookPlan(removed)
			require.NoError(t, err)
			require.True(t, changed)
		})
	}
}
