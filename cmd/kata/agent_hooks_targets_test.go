package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentHookCapabilitiesAndScopes(t *testing.T) {
	names := []string{"claude", "codex", "gemini", "copilot", "cursor", "qwen", "hermes", "droid", "antigravity", "amp", "opencode", "pi", "openclaw", "kimi-code", "kimi", "muse", "grok", "zcode"}
	for _, name := range names {
		capability, err := lookupAgentHookCapability(name)
		require.NoError(t, err)
		require.Equal(t, name, capability.Name)
		switch name {
		case "kimi", "grok":
			require.False(t, capability.Contract)
		default:
			require.True(t, capability.Contract)
		}
		switch name {
		case "antigravity", "amp", "opencode", "zcode":
			require.False(t, capability.AttentionEnd)
		default:
			require.True(t, capability.AttentionEnd)
		}
	}
	for _, alias := range []string{"factory", "agy", "antigravity-cli"} {
		_, err := lookupAgentHookCapability(alias)
		require.NoError(t, err)
	}
	_, err := lookupAgentHookCapability("unknown")
	require.Error(t, err)
	for _, target := range []string{"kimi-code", "kimi", "zcode", "hermes"} {
		_, err = agentHookScopePath(target, "project", t.TempDir())
		require.Error(t, err)
	}
	dir := t.TempDir()
	for target, want := range map[string]string{"claude": ".claude/settings.json", "codex": ".codex/hooks.json", "copilot": ".github/hooks/kata.json", "cursor": ".cursor/hooks.json", "gemini": ".gemini/settings.json", "qwen": ".qwen/settings.json", "droid": ".factory/hooks.json", "antigravity": ".agents/hooks.json", "pi": ".pi/extensions/kata.js"} {
		got, err := agentHookScopePath(target, "project", dir)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(dir, filepath.FromSlash(want)), got)
	}
}

func TestAgentHookPiScopePathMatchesNativeHome(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Setenv("PI_CODING_AGENT_DIR", "~/native-pi")
	got, err := agentHookScopePath("pi", "user", "")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "native-pi", "extensions", "kata.js"), got)
	t.Setenv("PI_CODING_AGENT_DIR", "relative-pi")
	_, err = agentHookScopePath("pi", "user", "")
	require.Error(t, err)
}

func TestAgentHookOpenClawAllSelectionUsesNativeProfile(t *testing.T) {
	for _, choice := range []string{"profile", "config", "home", "state"} {
		t.Run(choice, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			root := filepath.Join(home, ".openclaw-example")
			switch choice {
			case "profile":
				t.Setenv("OPENCLAW_PROFILE", "example")
			case "config":
				t.Setenv("OPENCLAW_CONFIG_PATH", filepath.Join(root, "openclaw.json"))
			case "home":
				t.Setenv("OPENCLAW_HOME", filepath.Join(home, "alternate"))
				root = filepath.Join(home, "alternate", ".openclaw")
			case "state":
				t.Setenv("OPENCLAW_STATE_DIR", "~/.openclaw-example")
			}
			require.NoError(t, os.MkdirAll(root, 0700))
			targets, err := selectNativeAgentHookTargets(nil, true, "user", "", t.TempDir())
			require.NoError(t, err)
			for _, target := range targets {
				if target.capability.Name == "openclaw" {
					require.Empty(t, target.skip)
					require.Equal(t, root, nativeAgentHookConfigRoot("openclaw", "user", target.path))
					return
				}
			}
			t.Fatal("OpenClaw target missing")
		})
	}
}

func TestAgentHookNativeFallbackPaths(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("PATH", "")
	for _, target := range []string{"amp", "opencode"} {
		for _, scope := range []string{"user", "project"} {
			t.Run(target+"/"+scope, func(t *testing.T) {
				root := filepath.Join(home, ".config", target)
				if scope == "project" {
					root = filepath.Join(workspace, "."+target)
				}
				directory := "plugins"
				if target == "opencode" {
					directory = "plugin"
				}
				expected := filepath.Join(root, directory, "kata-"+scope+".js")
				got, err := agentHookScopePath(target, scope, workspace)
				if err != nil || got != expected {
					t.Fatalf("fallback got %q %v want %q", got, err, expected)
				}
				if target == "opencode" {
					v2 := filepath.Join(root, "plugins", "kata-"+scope, "index.js")
					if err = os.MkdirAll(filepath.Dir(v2), 0700); err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(v2, []byte("export default {};\n"), 0600); err != nil {
						t.Fatal(err)
					}
					got, err = agentHookScopePath(target, scope, workspace)
					if err != nil || got != v2 {
						t.Fatalf("known v2 fallback got %q %v want %q", got, err, v2)
					}
				}
			})
		}
	}
}
