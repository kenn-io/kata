package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
	"gopkg.in/yaml.v3"
)

func nativeHookDocument(t *testing.T, agent agenthook.Agent, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: native fixture under TempDir.
	require.NoError(t, err)
	var document map[string]any
	if agent == agenthook.AgentHermes {
		require.NoError(t, yaml.Unmarshal(data, &document))
	} else {
		require.NoError(t, json.Unmarshal(data, &document))
	}
	return document
}

func nativeContractHandlers(t *testing.T, agent agenthook.Agent, path string) []map[string]any {
	t.Helper()
	doc := nativeHookDocument(t, agent, path)
	hooks := doc["hooks"].(map[string]any)
	event := "SessionStart"
	if agent == agenthook.AgentCursor {
		event = "sessionStart"
	}
	if agent == agenthook.AgentHermes {
		event = "pre_llm_call"
	}
	var handlers []map[string]any
	for _, raw := range hooks[event].([]any) {
		entry := raw.(map[string]any)
		if nested, ok := entry["hooks"].([]any); ok {
			for _, h := range nested {
				handler := h.(map[string]any)
				handler["matcher"] = entry["matcher"]
				handlers = append(handlers, handler)
			}
		} else {
			handlers = append(handlers, entry)
		}
	}
	return handlers
}

func installHookFixture(t *testing.T, agent agenthook.Agent, path, executable, marker string, hook agenthook.Hook) {
	t.Helper()
	arguments := []string{"--source", marker}
	if marker == legacyAgentContractHookSource {
		arguments = []string{"agent-hooks", "contract", string(agent), "--source", marker}
	} else if mode, ok := strings.CutPrefix(marker, legacyAttentionHookSource); ok {
		arguments = []string{"attention-hook", mode, "--source", marker}
	}
	_, err := agenthook.Install(agent, agenthook.InstallOptions{ConfigPath: path, Executable: executable, Arguments: arguments, Marker: "--source " + marker, Hooks: []agenthook.Hook{hook}})
	require.NoError(t, err)
}

func TestAgentHooksInstallAllProfiles(t *testing.T) {
	isolateAgentHookHomes(t)
	workspace := t.TempDir()
	t.Chdir(workspace)
	executable := filepath.Join(t.TempDir(), "stable path's kata.exe")
	require.NoError(t, os.WriteFile(executable, []byte("fixture"), 0o700)) //nolint:gosec // G306: owner-only executable fixture.
	for _, profile := range agenthook.Profiles() {
		if profile.Agent == agenthook.AgentDroid {
			continue
		}
		t.Run(string(profile.Agent), func(t *testing.T) {
			path, err := agenthook.ConfigPath(profile.Agent)
			require.NoError(t, err)
			hook := agenthook.Hook{Event: agenthook.EventSessionStart, Timeout: time.Second}
			if profile.Agent == agenthook.AgentHermes {
				hook.Event = agenthook.EventUserPromptSubmit
			}
			installHookFixture(t, profile.Agent, path, "foreign-before", "example-before", hook)
			out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "--contract-only", string(profile.Agent), "--executable", executable)
			require.NoError(t, err, stderr)
			if profile.Agent == agenthook.AgentCodex {
				require.Contains(t, out, "Codex runs new hooks only after you trust them: open Codex and run /hooks.")
			} else {
				require.NotContains(t, out, "Codex runs new hooks")
			}
			handlers := nativeContractHandlers(t, profile.Agent, path)
			require.Len(t, handlers, 2)
			commands, err := agenthook.BuildCommand(executable, "agent-hooks", "contract", string(profile.Agent), "--source", "kata-agent-contract-hook")
			require.NoError(t, err)
			owned := handlers[1]
			command := owned["command"]
			if profile.Agent == agenthook.AgentCopilot {
				command = owned["bash"]
				require.Equal(t, commands.PowerShell, owned["powershell"])
			}
			// Kit uses the native form except for Copilot's explicitly POSIX bash field.
			expected := commands.Native
			if profile.Agent == agenthook.AgentCopilot {
				expected = commands.POSIX
			}
			require.Equal(t, expected, command)
			timeoutKey := "timeout"
			expectedTimeout := float64(10)
			if profile.Agent == agenthook.AgentGemini || profile.Agent == agenthook.AgentQwen {
				expectedTimeout = 10000
			}
			if profile.Agent == agenthook.AgentCopilot {
				timeoutKey = "timeoutSec"
			}
			if profile.Agent == agenthook.AgentHermes {
				require.Equal(t, 10, owned[timeoutKey])
				require.NotContains(t, owned, "matcher")
			} else {
				require.Equal(t, expectedTimeout, owned[timeoutKey])
				if profile.Agent == agenthook.AgentCodex {
					require.Equal(t, "startup|resume|clear|compact", owned["matcher"])
				} else {
					// Lifecycle matchers are not portable regexes: Gemini compares
					// exact sources. An omitted matcher covers every session source.
					require.Empty(t, owned["matcher"])
				}
			}
			installHookFixture(t, profile.Agent, path, "foreign-after", "example-after", hook)
			before, err := os.ReadFile(path) //nolint:gosec // G304: native fixture under isolated temp HOME.
			require.NoError(t, err)
			stat, err := os.Stat(path)
			require.NoError(t, err)
			out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "--contract-only", string(profile.Agent), "--executable", executable)
			require.NoError(t, err, stderr)
			require.Contains(t, out, "unchanged")
			require.NotContains(t, out, "Codex runs")
			after, err := os.ReadFile(path) //nolint:gosec // G304: native fixture under isolated temp HOME.
			require.NoError(t, err)
			require.Equal(t, before, after)
			afterStat, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, stat.ModTime(), afterStat.ModTime())
			replacement := filepath.Join(t.TempDir(), "new-kata.exe")
			require.NoError(t, os.WriteFile(replacement, []byte("fixture"), 0o700)) //nolint:gosec // G306: executable fixture.
			_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "--contract-only", string(profile.Agent), "--executable", replacement)
			require.NoError(t, err, stderr)
			handlers = nativeContractHandlers(t, profile.Agent, path)
			require.Len(t, handlers, 3)
			for i, marker := range []string{"example-before", "example-after", "agent-hooks contract"} {
				command, _ := handlers[i]["command"].(string)
				if command == "" {
					command, _ = handlers[i]["bash"].(string)
				}
				require.Contains(t, command, marker)
			}
		})
	}
}

func TestAgentHooksInstallCompleteOwnedSet(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	for _, problem := range []string{"timeout", "platform", "duplicate", "other event"} {
		t.Run(problem, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			planned, err := agenthook.PlanInstall(agenthook.AgentCodex, agenthook.InstallOptions{
				ConfigPath: path, Executable: os.Args[0], Arguments: []string{"agent-hooks", "contract", "codex", "--source", legacyAgentContractHookSource},
				Marker: agentContractMarker, Hooks: []agenthook.Hook{contractRegistrationHook(agenthook.AgentCodex)},
			})
			require.NoError(t, err)
			var doc map[string]any
			require.NoError(t, json.Unmarshal(planned.Data, &doc))
			hooks := doc["hooks"].(map[string]any)
			groups := hooks["SessionStart"].([]any)
			handler := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
			switch problem {
			case "timeout":
				handler["timeout"] = 9
			case "platform":
				delete(handler, "commandWindows")
			case "duplicate":
				hooks["SessionStart"] = append(groups, groups[0])
			case "other event":
				hooks["Stop"] = groups
			}
			data, err := json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o600))
			out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "--contract-only", "codex", "--config", path, "--executable", os.Args[0])
			require.NoError(t, err, stderr)
			require.Contains(t, out, "re-trust")
			require.Contains(t, out, "/hooks")
			require.Len(t, nativeContractHandlers(t, agenthook.AgentCodex, path), 1)
			doc = nativeHookDocument(t, agenthook.AgentCodex, path)
			require.NotContains(t, doc["hooks"], "Stop")
		})
	}
}

func TestAgentHooksInstallPreflight(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path, err := agenthook.ConfigPath(agenthook.AgentCodex)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("{broken"), 0o600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "claude", "codex", "--executable", os.Args[0], "--json")
	require.ErrorContains(t, err, strconv.Quote(path))
	require.Empty(t, out)
	_, err = os.Stat(filepath.Join(home, "claude"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestAgentHooksInstallUsageAndCompletion(t *testing.T) {
	isolateAgentHookHomes(t)
	for _, args := range [][]string{{"--all", "claude"}, {"claude", "kimi", "--contract-only"}, {"unknown"}, {"--all", "--config", "x"}, {"--config", "x"}, {"claude", "codex", "--config", "x"}, {"claude", "--config="}, {"claude", "--executable="}} {
		out, stderr, err := executeAgentHook(t, unreadableHookInput{}, append([]string{"agent-hooks", "install"}, args...)...)
		require.Error(t, err, strings.Join(args, " "))
		require.Empty(t, out)
		require.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered), stderr)
	}
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "__complete", "agent-hooks", "install", "claude", "")
	require.NoError(t, err)
	require.Contains(t, out, "codex\n")
	require.Contains(t, out, "droid")
	require.NotContains(t, out, "claude\n")
	require.Contains(t, out, ":4")
}

func TestAgentHooksInstallAllAndWorkspaceHint(t *testing.T) {
	home := isolateAgentHookHomes(t)
	workspace := t.TempDir()
	t.Chdir(workspace)
	require.NoError(t, os.Mkdir(filepath.Join(home, "copilot"), 0o700))
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "--all", "--executable", os.Args[0], "--json")
	require.NoError(t, err, stderr)
	var report struct {
		Version int                 `json:"kata_api_version"`
		Action  string              `json:"action"`
		Results []agentHookMutation `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Equal(t, 1, report.Version)
	require.Equal(t, "install", report.Action)
	require.Len(t, report.Results, len(agentHookCapabilities()))
	for _, result := range report.Results {
		require.NotNil(t, result.Warnings)
		if result.Harness == "copilot" {
			require.True(t, result.Changed)
		} else {
			require.Equal(t, "skipped", result.State)
		}
	}
	_, err = os.Stat(filepath.Join(home, "gemini", ".gemini"))
	require.ErrorIs(t, err, os.ErrNotExist)
	workspaceConfig := filepath.Join(workspace, ".codex", "hooks.json")
	installHookFixture(t, agenthook.AgentCodex, workspaceConfig, "kata", legacyAgentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	before, err := os.ReadFile(workspaceConfig) //nolint:gosec // G304: selected workspace fixture under temp directory.
	require.NoError(t, err)
	out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "codex", "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	require.Contains(t, out, "run kata init --with-codex-hooks here to drop the workspace duplicate")
	after, err := os.ReadFile(workspaceConfig) //nolint:gosec // G304: selected workspace fixture under temp directory.
	require.NoError(t, err)
	require.Equal(t, before, after)
	out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "codex", "--config", workspaceConfig, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	require.NotContains(t, out, "workspace duplicate")
}

func TestAgentHooksInstallHermesMigratesEvent(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path, err := agenthook.ConfigPath(agenthook.AgentHermes)
	require.NoError(t, err)
	installHookFixture(t, agenthook.AgentHermes, path, "kata", legacyAgentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "--contract-only", "hermes", "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	document := nativeHookDocument(t, agenthook.AgentHermes, path)
	require.NotContains(t, document["hooks"], "on_session_start")
	require.Len(t, nativeContractHandlers(t, agenthook.AgentHermes, path), 1)
}

func TestAgentHooksInstallExecutableOverride(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	bin := t.TempDir()
	name := "example-kata"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable := filepath.Join(bin, name)
	require.NoError(t, os.WriteFile(executable, []byte("fixture"), 0o700)) //nolint:gosec // G306: executable fixture.
	t.Setenv("PATH", bin)
	for _, override := range []string{name, executable} {
		t.Run(override, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "hooks.json")
			_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "--contract-only", "codex", "--config", config, "--executable", override)
			require.NoError(t, err, stderr)
			status, err := readAgentHookStatus(agenthook.AgentCodex, config, "")
			require.NoError(t, err)
			require.Len(t, status.Entries, 1)
			require.Equal(t, executable, status.Entries[0].Executable)
			require.True(t, status.Entries[0].ExecutableExists)
		})
	}
	nonExecutable := filepath.Join(bin, "plain-file")
	require.NoError(t, os.WriteFile(nonExecutable, []byte("fixture"), 0o600))
	invalid := []string{filepath.Join(bin, "missing"), bin}
	if runtime.GOOS != "windows" {
		invalid = append(invalid, nonExecutable)
	}
	for _, override := range invalid {
		t.Run(override, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "hooks.json")
			_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "--contract-only", "codex", "--config", config, "--executable", override)
			require.ErrorContains(t, err, "executable")
			_, err = os.Stat(config)
			require.ErrorIs(t, err, os.ErrNotExist, "invalid executable must fail before writing hooks")
		})
	}
}

func TestAgentHooksInstallMalformedWorkspaceIsAdvisory(t *testing.T) {
	isolateAgentHookHomes(t)
	workspace := t.TempDir()
	t.Chdir(workspace)
	path := filepath.Join(workspace, ".codex", "hooks.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	broken := []byte(`{"hooks": [`)
	require.NoError(t, os.WriteFile(path, broken, 0o600))
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "--contract-only", "codex", "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	require.Contains(t, out, "installed")
	require.Contains(t, out, "inspect workspace hooks")
	userPath, err := agenthook.ConfigPath(agenthook.AgentCodex)
	require.NoError(t, err)
	require.Len(t, nativeContractHandlers(t, agenthook.AgentCodex, userPath), 1)
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated workspace fixture.
	require.NoError(t, err)
	require.Equal(t, broken, after)
}

func TestAgentHooksInstallTrackedDuplicateHint(t *testing.T) {
	isolateAgentHookHomes(t)
	workspace := t.TempDir()
	t.Chdir(workspace)
	runGit(t, workspace, "init", "-q")
	path := filepath.Join(workspace, ".codex", "hooks.json")
	installHookFixture(t, agenthook.AgentCodex, path, "kata", legacyAgentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	runGit(t, workspace, "add", ".codex/hooks.json")
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "codex", "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	require.Contains(t, out, "tracked")
	require.Contains(t, out, "keeps")
	require.NotContains(t, out, "to drop")
}
