package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestAgentHooksManagedAttentionHelpDisclosesGlobalScope(t *testing.T) {
	resetFlags(t)
	out, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "--help")
	require.NoError(t, err)
	require.Contains(t, out, "all Muse managed hooks")
	require.Contains(t, out, "KATA_AUTH_TOKEN")
}

func TestAgentHooksCLIProjectBundle(t *testing.T) {
	isolateAgentHookHomes(t)
	dir := t.TempDir()
	resetFlags(t)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "codex", "--scope", "project", "--attention", "--workspace", dir, "--executable", os.Args[0])
	require.NoError(t, err)
	path := filepath.Join(dir, ".codex", "hooks.json")
	entries, err := inspectAgentHookEntries(agenthook.AgentCodex, path)
	require.NoError(t, err)
	contract, start, end := kitAgentHookConfigured(agenthook.AgentCodex, entries)
	require.True(t, contract)
	require.True(t, start)
	require.True(t, end)
	before, err := os.ReadFile(path) //nolint:gosec // G304: isolated native configuration fixture under TempDir.
	require.NoError(t, err)
	resetFlags(t)
	_, _, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "codex", "--scope", "project", "--workspace", dir, "--executable", os.Args[0])
	require.NoError(t, err)
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated native configuration fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
	resetFlags(t)
	_, _, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "uninstall", "codex", "--scope", "project", "--workspace", dir, "--contract-only")
	require.NoError(t, err)
	entries, err = inspectAgentHookEntries(agenthook.AgentCodex, path)
	require.NoError(t, err)
	contract, start, end = kitAgentHookConfigured(agenthook.AgentCodex, entries)
	require.False(t, contract)
	require.True(t, start)
	require.True(t, end)
	resetFlags(t)
	_, _, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "uninstall", "codex", "--scope", "project", "--attention", "--workspace", dir)
	require.NoError(t, err)
	entries, err = inspectAgentHookEntries(agenthook.AgentCodex, path)
	require.NoError(t, err)
	contract, start, end = kitAgentHookConfigured(agenthook.AgentCodex, entries)
	require.False(t, contract)
	require.False(t, start)
	require.False(t, end)
}

func TestAgentHooksCLIPrevalidateAllTargets(t *testing.T) {
	home := isolateAgentHookHomes(t)
	resetFlags(t)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "claude", "hermes", "--scope", "project", "--workspace", home, "--executable", os.Args[0])
	require.ErrorContains(t, err, "no verified project")
	_, err = os.Stat(filepath.Join(home, ".claude", "settings.json"))
	require.True(t, os.IsNotExist(err))
}

func TestAgentHooksCLISourceDataAndAttentionOnlyArtifact(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "pi-runtime"))
	resetFlags(t)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "pi", "--attention", "--source", "prompt with spaces.md", "--executable", os.Args[0])
	require.NoError(t, err)
	path := filepath.Join(home, "pi-runtime", "extensions", "kata.js")
	data, err := os.ReadFile(path) //nolint:gosec // G304: generated configuration fixture stays inside the isolated test home.
	require.NoError(t, err)
	meta, err := parsePiAgentHookMetadata(data)
	require.NoError(t, err)
	wantSource, err := filepath.Abs("prompt with spaces.md")
	require.NoError(t, err)
	require.Equal(t, wantSource, meta.Source)
	resetFlags(t)
	_, _, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "uninstall", "pi", "--contract-only")
	require.NoError(t, err)
	data, err = os.ReadFile(path) //nolint:gosec // G304: generated configuration fixture stays inside the isolated test home.
	require.NoError(t, err)
	meta, err = parsePiAgentHookMetadata(data)
	require.NoError(t, err)
	require.False(t, meta.Contract)
	require.True(t, meta.Attention)
	resetFlags(t)
	_, _, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "claude", "--source", "custom.md", "--executable", os.Args[0])
	require.ErrorContains(t, err, "--source")
}

func TestAgentHooksCLIUserSourceIsAnchoredToInstallWorkingDirectory(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installDir := t.TempDir()
	source := filepath.Join("prompts", "contract.md")
	require.NoError(t, os.Mkdir(filepath.Join(installDir, "prompts"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(installDir, source), []byte("custom contract"), 0600))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "pi-runtime"))
	t.Chdir(installDir)
	resetFlags(t)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "pi", "--attention", "--source", source, "--executable", os.Args[0])
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(home, "pi-runtime", "extensions", "kata.js")) //nolint:gosec // G304: generated configuration fixture stays inside the isolated test home.
	require.NoError(t, err)
	meta, err := parsePiAgentHookMetadata(data)
	require.NoError(t, err)
	want, err := filepath.Abs(source)
	require.NoError(t, err)
	require.Equal(t, want, meta.Source)
}

func TestAgentHooksCLIProjectSourceStaysWorkspaceRelative(t *testing.T) {
	isolateAgentHookHomes(t)
	workspace := t.TempDir()
	t.Chdir(t.TempDir())
	source := filepath.Join("prompts", "contract.md")
	resetFlags(t)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "pi", "--scope", "project", "--workspace", workspace, "--attention", "--source", source, "--executable", os.Args[0])
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(workspace, ".pi", "extensions", "kata.js")) //nolint:gosec // G304: generated configuration fixture stays in TempDir.
	require.NoError(t, err)
	meta, err := parsePiAgentHookMetadata(data)
	require.NoError(t, err)
	require.Equal(t, source, meta.Source)
}

func TestAgentHooksCLIStatusConfiguredCapabilities(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "pi-runtime"))
	resetFlags(t)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "pi", "--attention", "--executable", os.Args[0])
	require.NoError(t, err)
	path := filepath.Join(home, "pi-runtime", "extensions", "kata.js")
	before, err := os.ReadFile(path) //nolint:gosec // G304: isolated native configuration fixture under TempDir.
	require.NoError(t, err)
	resetFlags(t)
	out, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "status", "pi", "--json")
	require.NoError(t, err)
	var report struct {
		Harnesses []struct {
			Scope      string `json:"scope"`
			Configured struct {
				Contract       bool `json:"contract"`
				AttentionStart bool `json:"attention_start"`
				AttentionEnd   bool `json:"attention_end"`
			} `json:"configured"`
			Capabilities agentHookCapability `json:"capabilities"`
		} `json:"harnesses"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Harnesses, 1)
	require.Equal(t, "user", report.Harnesses[0].Scope)
	require.True(t, report.Harnesses[0].Configured.Contract)
	require.True(t, report.Harnesses[0].Configured.AttentionStart)
	require.True(t, report.Harnesses[0].Configured.AttentionEnd)
	require.Equal(t, "pi", report.Harnesses[0].Capabilities.Name)
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated native configuration fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestAgentHooksCLIExtraNativeContract(t *testing.T) {
	resetFlags(t)
	out, _, err := executeAgentHook(t, strings.NewReader(`{"session_id":"one","hook_event_name":"SessionStart","cwd":"/example"}`), "agent-hook", "contract", "droid")
	require.NoError(t, err)
	var result struct {
		Hook struct {
			Event string `json:"hookEventName"`
			Text  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Equal(t, "SessionStart", result.Hook.Event)
	require.NotEmpty(t, result.Hook.Text)
	resetFlags(t)
	_, _, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "contract", "pi")
	require.ErrorContains(t, err, "native extension")
}

func TestAgentHooksCLIAllCapabilityGaps(t *testing.T) {
	home := isolateAgentHookHomes(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".grok"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".config", "muse"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "claude"), 0700))
	resetFlags(t)
	out, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "--all", "--contract-only", "--executable", os.Args[0], "--json")
	require.NoError(t, err)
	var report struct {
		Results []agentHookMutation `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	for _, result := range report.Results {
		if result.Harness == "grok" {
			require.Equal(t, "skipped", result.State)
			require.Contains(t, result.Reason, "attention only")
		}
	}
	resetFlags(t)
	out, _, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "--all", "--attention", "--executable", os.Args[0], "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	for _, result := range report.Results {
		if result.Harness == "muse" {
			require.Equal(t, "partial", result.State)
			require.Contains(t, result.Reason, "--managed-attention")
		}
	}
}

func TestAgentHooksCLIPluginProviders(t *testing.T) {
	for _, target := range []string{"amp", "opencode"} {
		t.Run(target, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			args := []string{"agent-hook", "install", target, "--attention", "--executable", os.Args[0], "--json"}
			if target == "opencode" {
				t.Setenv("PATH", t.TempDir())
				args = append(args, "--api", "v2")
			}
			resetFlags(t)
			_, _, err := executeAgentHook(t, strings.NewReader(""), args...)
			require.NoError(t, err)
			resetFlags(t)
			out, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "status", target, "--json")
			require.NoError(t, err)
			var report struct {
				Harnesses []struct {
					Configured struct {
						Contract bool `json:"contract"`
						Start    bool `json:"attention_start"`
						End      bool `json:"attention_end"`
					} `json:"configured"`
				} `json:"harnesses"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &report))
			require.Len(t, report.Harnesses, 1)
			require.True(t, report.Harnesses[0].Configured.Contract)
			require.True(t, report.Harnesses[0].Configured.Start)
			require.False(t, report.Harnesses[0].Configured.End)
			resetFlags(t)
			_, _, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "uninstall", target, "--attention", "--json")
			require.NoError(t, err)
			plan, err := planPluginAgentHooks(nativeAgentHookOptions{Agent: target, Scope: "user", Home: home}, target, true)
			require.NoError(t, err)
			require.False(t, plan.CurrentContract)
			require.False(t, plan.CurrentAttentionStart)
		})
	}
}

func TestAgentHooksCLIStatusRestrictiveMatcher(t *testing.T) {
	isolateAgentHookHomes(t)
	workspace := t.TempDir()
	user := filepath.Join(t.TempDir(), "settings.json")
	content := []byte(`{"hooks":{"SessionStart":[{"matcher":"^never$","hooks":[{"command":"kata agent-hook contract claude"}]}]}}`)
	require.NoError(t, os.WriteFile(user, content, 0600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, ".claude"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".claude", "settings.json"), content, 0600))
	resetFlags(t)
	out, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "status", "claude", "--config", user, "--workspace", workspace, "--json")
	require.NoError(t, err)
	var report struct {
		Harnesses []struct {
			User struct {
				Present bool `json:"present"`
			} `json:"user"`
			Configured struct {
				Contract bool `json:"contract"`
			} `json:"configured"`
			Duplicate bool `json:"duplicate"`
		} `json:"harnesses"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Harnesses, 1)
	require.False(t, report.Harnesses[0].User.Present)
	require.False(t, report.Harnesses[0].Configured.Contract)
	require.False(t, report.Harnesses[0].Duplicate)
}

func TestAgentHooksCLIStatusUnmanageableNativeConfig(t *testing.T) {
	for _, target := range []string{"muse", "openclaw"} {
		t.Run(target, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			path := filepath.Join(home, ".config", "muse", "settings.json")
			content := []byte(`{"theme":"authored"}`)
			if target == "openclaw" {
				path = filepath.Join(home, ".openclaw", "openclaw.json")
				content = []byte(`{"$include":"authored.json"}`)
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
			require.NoError(t, os.WriteFile(path, content, 0600))
			for _, names := range [][]string{nil, {target}} {
				resetFlags(t)
				out, diagnostic, err := executeAgentHook(t, strings.NewReader(""), append(append([]string{"agent-hook", "status", "--scope", "user"}, names...), "--json")...)
				require.Error(t, err)
				require.Empty(t, out)
				require.ErrorContains(t, err, strconv.Quote(path))
				var failure struct {
					Error struct {
						Data json.RawMessage `json:"data"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(diagnostic), &failure))
				var report struct {
					Harnesses []struct {
						Harness         string              `json:"harness"`
						InspectionError string              `json:"inspection_error"`
						Capabilities    agentHookCapability `json:"capabilities"`
					} `json:"harnesses"`
				}
				require.NoError(t, json.Unmarshal(failure.Error.Data, &report))
				if len(names) == 0 {
					require.Len(t, report.Harnesses, 18)
				} else {
					require.Len(t, report.Harnesses, 1)
				}
				found := false
				for _, row := range report.Harnesses {
					if row.Harness == target {
						found = true
						require.NotEmpty(t, row.InspectionError)
						require.Equal(t, target, row.Capabilities.Name)
					}
				}
				require.True(t, found)
			}
			after, err := os.ReadFile(path) //nolint:gosec // G304: isolated authored configuration fixture under TempDir.
			require.NoError(t, err)
			require.Equal(t, content, after)
		})
	}
}

func TestAgentHooksCLIStatusRejectsUnsupportedConfigFlag(t *testing.T) {
	isolateAgentHookHomes(t)
	for _, args := range [][]string{{"pi"}, {"amp"}, {"opencode", "--scope", "project"}} {
		resetFlags(t)
		_, _, err := executeAgentHook(t, strings.NewReader(""), append(append([]string{"agent-hook", "status"}, args...), "--config", filepath.Join(t.TempDir(), "settings.json"), "--json")...)
		require.Error(t, err)
	}
}
