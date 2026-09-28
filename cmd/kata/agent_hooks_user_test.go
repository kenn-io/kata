package main

import (
	"os"
	"path/filepath"
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
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1")
	return home
}

func TestAgentHooksTargetValidation(t *testing.T) {
	isolateAgentHookHomes(t)
	for _, tc := range []struct {
		names  []string
		all    bool
		config string
	}{
		{nil, false, ""}, {[]string{"claude"}, true, ""},
		{[]string{"claude", "droid"}, false, ""}, {[]string{"unknown"}, false, ""},
		{nil, true, "override"}, {[]string{"claude", "codex"}, false, "override"},
		{nil, false, "override"},
	} {
		_, err := selectAgentHookTargets(tc.names, tc.all, tc.config)
		require.Error(t, err)
	}
	targets, err := selectAgentHookTargets([]string{"codex", "CODEX"}, false, "")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, agenthook.AgentCodex, targets[0].Agent)
	targets, err = selectAgentHookTargets([]string{"codex"}, false, "override")
	require.NoError(t, err)
	absolute, err := filepath.Abs("override")
	require.NoError(t, err)
	require.Equal(t, absolute, targets[0].ConfigPath)
}

func TestAgentHooksAllActualConfigRoots(t *testing.T) {
	home := isolateAgentHookHomes(t)
	require.NoError(t, os.Mkdir(filepath.Join(home, "copilot"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(home, "gemini"), 0o700))
	targets, err := selectAgentHookTargets(nil, true, "")
	require.NoError(t, err)
	require.Len(t, targets, len(agenthook.Profiles()))
	for _, target := range targets {
		switch target.Agent {
		case agenthook.AgentCopilot:
			require.Empty(t, target.SkipReason)
		case agenthook.AgentDroid:
			require.Equal(t, "no SessionStart", target.SkipReason)
		default:
			require.Equal(t, "not installed", target.SkipReason)
		}
	}
	require.NoError(t, os.Mkdir(filepath.Join(home, "gemini", ".gemini"), 0o700))
	targets, err = selectAgentHookTargets(nil, true, "")
	require.NoError(t, err)
	for _, target := range targets {
		if target.Agent == agenthook.AgentGemini {
			require.Empty(t, target.SkipReason)
		}
	}
}

func TestAgentHooksKitContractRegistration(t *testing.T) {
	for _, profile := range sessionStartProfiles() {
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
			Arguments: []string{"agent-hooks", "contract", string(profile.Agent), "--source", agentContractHookSource},
			Marker:    "--source " + agentContractHookSource, Hooks: []agenthook.Hook{hook},
		})
		require.NoError(t, err)
	}
}
