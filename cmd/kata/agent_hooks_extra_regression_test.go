package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtraAbsentRemovalDoesNotCreateArtifacts(t *testing.T) {
	for _, target := range []string{"droid", "antigravity", "zcode", "kimi-code", "kimi", "muse", "grok"} {
		opts := extraOptions(t, target)
		plan, err := planExtraAgentHooks(opts, true)
		require.NoError(t, err)
		changed, err := publishNativeAgentHookPlan(plan)
		require.NoError(t, err)
		require.False(t, changed, target)
		_, err = os.Stat(plan.Path)
		require.True(t, os.IsNotExist(err), target)
	}
}

func TestExtraRemovalWithNoOwnedHandlersKeepsOriginalBytes(t *testing.T) {
	for _, target := range []string{"droid", "antigravity", "zcode", "kimi-code", "kimi", "muse", "grok"} {
		opts := extraOptions(t, target)
		opts.ConfigPath = filepath.Join(opts.Home, "native")
		data := []byte("{  }\n")
		if target == "muse" {
			data = []byte("{ \"schema_version\": 1 }\n")
		}
		if target == "kimi-code" || target == "kimi" {
			data = []byte("# foreign settings\nmodel='example-model'\n")
		}
		require.NoError(t, os.WriteFile(opts.ConfigPath, data, 0600))
		plan, err := planExtraAgentHooks(opts, true)
		require.NoError(t, err)
		changed, err := publishNativeAgentHookPlan(plan)
		require.NoError(t, err)
		require.False(t, changed, target)
		got, err := os.ReadFile(opts.ConfigPath)
		require.NoError(t, err)
		require.Equal(t, data, got)
	}
}

func TestDroidFallbackAppearanceInvalidatesFreshPlan(t *testing.T) {
	opts := extraOptions(t, "droid")
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	fallback := filepath.Join(opts.Home, ".factory", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(fallback), 0700))
	require.NoError(t, os.WriteFile(fallback, []byte(`{"hooks":{"Stop":[{"hooks":[{"command":"echo new foreign"}]}]}}`), 0600))
	_, err = publishNativeAgentHookPlan(plan)
	require.ErrorContains(t, err, "changed after planning")
	_, err = os.Stat(plan.Path)
	require.True(t, os.IsNotExist(err))
}

func TestExtraZCodeUsesArgvAndEnablesNativeHooks(t *testing.T) {
	opts := extraOptions(t, "zcode")
	opts.ConfigPath = filepath.Join(opts.Home, "config.json")
	opts.Executable = "/opt/example path/kata'$(echo literal)"
	require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(`{"theme":"dark","hooks":{"timeoutMs":30000,"events":{}}}`), 0600))
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(plan.Path)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(data, &root))
	hooks := root["hooks"].(map[string]any)
	require.Equal(t, true, hooks["enabled"])
	events := hooks["events"].(map[string]any)
	groups := events["SessionStart"].([]any)
	for i, want := range [][]any{{"agent-hook", "contract", "zcode", "--source", "kata-agent-contract-hook"}, {"agent-hook", "attention-native", "zcode", "start", "--source", "kata-agent-hook-start"}} {
		handler := groups[i].(map[string]any)["hooks"].([]any)[0].(map[string]any)
		require.Equal(t, "process", handler["type"])
		require.Equal(t, opts.Executable, handler["command"])
		require.Equal(t, want, handler["args"])
		require.Equal(t, float64(10000), handler["timeoutMs"])
	}
	require.NotContains(t, events, "SessionEnd")
}

func TestExtraAgentHonorsNativeUserEnvironmentRoots(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	for _, tc := range []struct{ target, variable, suffix string }{
		{"kimi-code", "KIMI_CODE_HOME", "config.toml"},
		{"grok", "GROK_HOME", "hooks/kata.json"},
		{"muse", "XDG_CONFIG_HOME", "muse/settings.json"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv(tc.variable, root)
			opts := extraOptions(t, tc.target)
			opts.Home = home
			if tc.target == "muse" {
				opts.Attention = false
			}
			plan, err := planExtraAgentHooks(opts, false)
			require.NoError(t, err)
			require.Equal(t, filepath.Join(root, filepath.FromSlash(tc.suffix)), plan.Path)
			opts.Home = t.TempDir()
			plan, err = planExtraAgentHooks(opts, false)
			require.NoError(t, err)
			require.NotEqual(t, filepath.Join(root, filepath.FromSlash(tc.suffix)), plan.Path)
		})
	}
}

func TestExtraPlansNeverClaimUnsupportedPreexistingDirections(t *testing.T) {
	for _, target := range []string{"kimi", "grok", "antigravity", "zcode"} {
		opts := extraOptions(t, target)
		opts.ConfigPath = filepath.Join(opts.Home, "native")
		var input string
		switch target {
		case "kimi":
			input = "[[hooks]]\nevent='SessionStart'\ncommand='kata agent-hook contract kimi'\n"
		case "grok":
			input = `{"hooks":{"SessionStart":[{"hooks":[{"command":"kata agent-hook contract grok"}]}]}}`
		case "antigravity":
			input = `{"old":{"PreInvocation":[{"command":"kata agent-hook attention-native antigravity start"}]}}`
		case "zcode":
			input = `{"hooks":{"events":{"SessionEnd":[{"hooks":[{"type":"process","command":"kata","args":["agent-hook","attention-native","zcode","end"]}]}]}}}`
		}
		require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(input), 0600))
		plan, err := planExtraAgentHooks(opts, false)
		require.NoError(t, err)
		if target == "kimi" || target == "grok" {
			require.False(t, plan.Contract)
			require.False(t, plan.CurrentContract)
		}
		if target == "antigravity" {
			require.False(t, plan.AttentionStart)
		}
		if target == "zcode" {
			require.False(t, plan.AttentionEnd)
		}
	}
}
