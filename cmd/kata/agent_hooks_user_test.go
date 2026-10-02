package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func isolateAgentHookHomes(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, p := range agenthook.Profiles() {
		if p.ConfigEnvironment != "" {
			t.Setenv(p.ConfigEnvironment, filepath.Join(home, string(p.Agent)))
		}
	}
	for _, name := range []string{"XDG_CONFIG_HOME", "PI_CODING_AGENT_DIR", "OPENCLAW_STATE_DIR", "OPENCLAW_CONFIG_PATH", "OPENCLAW_PROFILE", "OPENCLAW_HOME", "GROK_HOME", "KIMI_CODE_HOME"} {
		t.Setenv(name, "")
	}
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1")
	return home
}

func TestAgentHooksKitContractRegistration(t *testing.T) {
	for _, profile := range agenthook.Profiles() {
		if !slices.Contains(profile.SupportedEvents, agenthook.EventSessionStart) {
			continue
		}
		hook := contractRegistrationHook(profile.Agent)
		require.Equal(t, 10_000_000_000, int(hook.Timeout))
		if profile.Agent == agenthook.AgentHermes {
			require.Equal(t, agenthook.EventUserPromptSubmit, hook.Event)
			require.Empty(t, hook.Matcher)
		} else {
			require.Equal(t, agenthook.EventSessionStart, hook.Event)
			if profile.Agent == agenthook.AgentCodex {
				require.Equal(t, "startup|resume|clear|compact", hook.Matcher)
			} else {
				require.Empty(t, hook.Matcher)
			}
		}
		_, err := agenthook.PlanInstall(profile.Agent, agenthook.InstallOptions{
			ConfigPath: filepath.Join(t.TempDir(), profile.ConfigFilename), Executable: os.Args[0],
			Arguments: []string{"agent-hook", "contract", string(profile.Agent), "--source", legacyAgentContractHookSource},
			Marker:    "--source " + legacyAgentContractHookSource, Hooks: []agenthook.Hook{hook},
		})
		require.NoError(t, err)
	}
}
