package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func agentHooksStatusJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, append([]string{"agent-hooks", "status", "--json"}, args...)...)
	require.NoError(t, err, stderr)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	return report
}

func TestAgentHooksStatusSchemaAndExecutable(t *testing.T) {
	isolateAgentHookHomes(t)
	workspace := t.TempDir()
	executable := filepath.Join(t.TempDir(), "example path's kata")
	require.NoError(t, os.WriteFile(executable, []byte("fixture"), 0o700)) //nolint:gosec // G306: executable fixture requires execute permission.
	userConfig := filepath.Join(t.TempDir(), "selected-home", "hooks.json")
	installHookFixture(t, agenthook.AgentCodex, userConfig, executable, agentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	workspaceConfig := filepath.Join(workspace, ".codex", "hooks.json")
	installHookFixture(t, agenthook.AgentCodex, workspaceConfig, "missing-kata", agentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	installHookFixture(t, agenthook.AgentCodex, workspaceConfig, "missing-kata", attentionHookSource+"start", agenthook.Hook{Event: agenthook.EventSessionStart})
	claudeConfig := filepath.Join(workspace, ".claude", "settings.json")
	installHookFixture(t, agenthook.AgentClaude, claudeConfig, "missing-kata", attentionHookSource+"end", agenthook.Hook{Event: agenthook.EventSessionEnd})
	before, err := os.ReadFile(userConfig) //nolint:gosec // G304: isolated user/workspace config fixture under TempDir.
	require.NoError(t, err)
	report := agentHooksStatusJSON(t, "codex", "--config", userConfig, "--workspace", workspace)
	require.ElementsMatch(t, []string{"kata_api_version", "harnesses", "workspace", "warnings"}, agentHookStatusKeys(report))
	require.Equal(t, float64(1), report["kata_api_version"])
	harnesses := report["harnesses"].([]any)
	require.Len(t, harnesses, 1)
	harness := harnesses[0].(map[string]any)
	require.ElementsMatch(t, []string{"harness", "user", "duplicate", "overlap"}, agentHookStatusKeys(harness))
	require.Equal(t, "codex", harness["harness"])
	require.Equal(t, true, harness["duplicate"])
	require.Equal(t, false, harness["overlap"])
	user := harness["user"].(map[string]any)
	require.ElementsMatch(t, []string{"config_path", "present", "entries"}, agentHookStatusKeys(user))
	require.Equal(t, userConfig, user["config_path"])
	require.Equal(t, true, user["present"])
	entries := user["entries"].([]any)
	require.Len(t, entries, 1)
	entry := entries[0].(map[string]any)
	require.ElementsMatch(t, []string{"event", "group_index", "handler_index", "matcher", "command", "executable", "executable_exists", "kind"}, agentHookStatusKeys(entry))
	require.Equal(t, executable, entry["executable"])
	require.Equal(t, true, entry["executable_exists"])
	require.Equal(t, "contract", entry["kind"])
	ws := report["workspace"].(map[string]any)
	require.ElementsMatch(t, []string{"path", "claude", "codex", "committed_guidance"}, agentHookStatusKeys(ws))
	require.Equal(t, workspace, ws["path"])
	require.Equal(t, []any{}, ws["committed_guidance"])
	require.Len(t, ws["codex"].(map[string]any)["entries"], 2)
	require.Len(t, ws["claude"].(map[string]any)["entries"], 1)
	after, err := os.ReadFile(userConfig) //nolint:gosec // G304: isolated user/workspace config fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoError(t, os.Remove(executable))
	report = agentHooksStatusJSON(t, "codex", "--config", userConfig, "--workspace", workspace)
	entry = report["harnesses"].([]any)[0].(map[string]any)["user"].(map[string]any)["entries"].([]any)[0].(map[string]any)
	require.Equal(t, false, entry["executable_exists"])
	for _, mode := range []string{"", "--agent"} {
		args := []string{"agent-hooks", "status", "codex", "--config", userConfig, "--workspace", workspace}
		if mode != "" {
			args = append(args, mode)
		}
		out, stderr, err := executeAgentHook(t, unreadableHookInput{}, args...)
		require.NoError(t, err, stderr)
		if mode == "" {
			require.Contains(t, out, "Workspace: ")
			require.Contains(t, out, "Command: ")
			require.Contains(t, out, "Executable: ")
		}
		wantPath := userConfig
		if mode == "--agent" && strings.ContainsAny(userConfig, " \t\"\\") {
			wantPath = strconv.Quote(userConfig)
		}
		for _, text := range []string{"duplicate", wantPath, "contract", "attention", "false"} {
			require.Contains(t, out, text)
		}
	}
}

func agentHookStatusKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func TestAgentHookCommandForPlatformSelectsNativeVariant(t *testing.T) {
	windowsCommand := `"C:\Program Files\Kata\kata.exe" agent-hooks contract codex --source kata-agent-contract-hook`
	for _, tc := range []struct {
		name   string
		agent  agenthook.Agent
		entry  agentHookEntry
		goos   string
		want   string
		wantPS bool
	}{
		{
			name:  "Codex on Windows",
			agent: agenthook.AgentCodex,
			entry: agentHookEntry{Command: `"/usr/local/bin/kata" agent-hooks contract codex --source kata-agent-contract-hook`, Fields: map[string]any{"commandWindows": windowsCommand}},
			goos:  "windows",
			want:  windowsCommand,
		},
		{
			name:   "Copilot on Windows",
			agent:  agenthook.AgentCopilot,
			entry:  agentHookEntry{Command: "kata agent-hooks contract copilot", Fields: map[string]any{"bash": "kata agent-hooks contract copilot", "powershell": "& 'C:\\Program Files\\Kata\\kata.exe' agent-hooks contract copilot"}},
			goos:   "windows",
			want:   "& 'C:\\Program Files\\Kata\\kata.exe' agent-hooks contract copilot",
			wantPS: true,
		},
		{
			name:  "Codex on Unix",
			agent: agenthook.AgentCodex,
			entry: agentHookEntry{Command: `"/usr/local/bin/kata" agent-hooks contract codex --source kata-agent-contract-hook`, Fields: map[string]any{"commandWindows": windowsCommand}},
			goos:  "linux",
			want:  `"/usr/local/bin/kata" agent-hooks contract codex --source kata-agent-contract-hook`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, powershell := agentHookCommandForPlatform(tc.agent, tc.entry, tc.goos)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.wantPS, powershell)
		})
	}
}

func TestAgentHooksStatusCommittedOverlap(t *testing.T) {
	isolateAgentHookHomes(t)
	config := filepath.Join(t.TempDir(), "hooks.json")
	installHookFixture(t, agenthook.AgentCodex, config, "kata", agentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	for _, state := range []string{"untracked", "staged", "committed", "modified", "ordinary"} {
		t.Run(state, func(t *testing.T) {
			repo := t.TempDir()
			runGit(t, repo, "init", "-q")
			runGit(t, repo, "config", "user.name", "Example Actor")
			runGit(t, repo, "config", "user.email", "actor@example.invalid")
			guidance := agentsBlockBegin + "\nfixture\n" + agentsBlockEnd + "\n"
			if state == "ordinary" {
				guidance = "ordinary guidance\n"
			}
			path := filepath.Join(repo, "AGENTS.md")
			require.NoError(t, os.WriteFile(path, []byte(guidance), 0o600))
			if state != "untracked" {
				runGit(t, repo, "add", "AGENTS.md")
			}
			committed := state == "committed" || state == "modified" || state == "ordinary"
			if committed {
				runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-qm", "fixture guidance")
			}
			if state == "modified" {
				require.NoError(t, os.WriteFile(path, []byte("local edit\n"), 0o600))
			}
			before, err := os.ReadFile(path) //nolint:gosec // G304: isolated user/workspace config fixture under TempDir.
			require.NoError(t, err)
			report := agentHooksStatusJSON(t, "codex", "--config", config, "--workspace", repo)
			harness := report["harnesses"].([]any)[0].(map[string]any)
			expected := state == "committed" || state == "modified"
			require.Equal(t, expected, harness["overlap"])
			guidancePaths := report["workspace"].(map[string]any)["committed_guidance"]
			if expected {
				require.Equal(t, []any{"AGENTS.md"}, guidancePaths)
			} else {
				require.Equal(t, []any{}, guidancePaths)
			}
			after, err := os.ReadFile(path) //nolint:gosec // G304: isolated user/workspace config fixture under TempDir.
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestAgentHooksStatusMissingAndSameFile(t *testing.T) {
	isolateAgentHookHomes(t)
	workspace := t.TempDir()
	report := agentHooksStatusJSON(t, "--workspace", workspace)
	require.Len(t, report["harnesses"], 7)
	for _, raw := range report["harnesses"].([]any) {
		harness := raw.(map[string]any)
		require.NotEqual(t, "droid", harness["harness"])
		require.Equal(t, false, harness["duplicate"])
		user := harness["user"].(map[string]any)
		require.Equal(t, false, user["present"])
		require.Equal(t, []any{}, user["entries"])
	}
	config := filepath.Join(workspace, ".codex", "hooks.json")
	installHookFixture(t, agenthook.AgentCodex, config, "kata", agentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	report = agentHooksStatusJSON(t, "codex", "--config", config, "--workspace", workspace)
	require.Equal(t, false, report["harnesses"].([]any)[0].(map[string]any)["duplicate"])
}

func TestAgentHooksStatusUsage(t *testing.T) {
	isolateAgentHookHomes(t)
	for _, args := range [][]string{{"claude", "codex"}, {"droid"}, {"unknown"}, {"--config", "example"}, {"codex", "--config="}, {"--all"}} {
		out, stderr, err := executeAgentHook(t, unreadableHookInput{}, append([]string{"agent-hooks", "status"}, args...)...)
		require.Error(t, err, strings.Join(args, " "))
		require.Empty(t, out)
		require.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered), stderr)
	}
}

func TestAgentHooksStatusHermesEventAndNestedGuidance(t *testing.T) {
	isolateAgentHookHomes(t)
	repo := t.TempDir()
	workspace := filepath.Join(repo, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.name", "Example Actor")
	runGit(t, repo, "config", "user.email", "actor@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "CLAUDE.md"), []byte(agentsBlockBegin+"\nfixture\n"+agentsBlockEnd), 0o600))
	runGit(t, repo, "add", "workspace/CLAUDE.md")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
	config, err := agenthook.ConfigPath(agenthook.AgentHermes)
	require.NoError(t, err)
	installHookFixture(t, agenthook.AgentHermes, config, "kata", agentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	report := agentHooksStatusJSON(t, "hermes", "--workspace", workspace)
	harness := report["harnesses"].([]any)[0].(map[string]any)
	require.Equal(t, false, harness["user"].(map[string]any)["present"])
	require.Equal(t, false, harness["overlap"])
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--executable", os.Args[0], "--workspace", workspace)
	require.NoError(t, err, stderr)
	report = agentHooksStatusJSON(t, "hermes", "--workspace", workspace)
	harness = report["harnesses"].([]any)[0].(map[string]any)
	require.Equal(t, true, harness["user"].(map[string]any)["present"])
	require.Equal(t, true, harness["overlap"])
	entry := harness["user"].(map[string]any)["entries"].([]any)[0].(map[string]any)
	require.Equal(t, "pre_llm_call", entry["event"])
	require.Equal(t, "", entry["matcher"])
	require.Equal(t, []any{"CLAUDE.md"}, report["workspace"].(map[string]any)["committed_guidance"])
}

func TestAgentHooksStatusWorkspaceFailuresAreAdvisory(t *testing.T) {
	for _, problem := range []string{"claude config", "codex config", "no git", "non-repository"} {
		t.Run(problem, func(t *testing.T) {
			isolateAgentHookHomes(t)
			workspace := t.TempDir()
			t.Chdir(workspace)
			config := filepath.Join(t.TempDir(), "hooks.json")
			installHookFixture(t, agenthook.AgentCodex, config, os.Args[0], agentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
			warning := "committed guidance"
			switch problem {
			case "no git":
				t.Setenv("PATH", t.TempDir())
			case "claude config", "codex config":
				dir, name := ".claude", "settings.json"
				if problem == "codex config" {
					dir, name = ".codex", "hooks.json"
				}
				require.NoError(t, os.Mkdir(filepath.Join(workspace, dir), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(workspace, dir, name), []byte(`{"hooks": [`), 0o600))
				warning = name
			}
			report := agentHooksStatusJSON(t, "codex", "--config", config)
			harness := report["harnesses"].([]any)[0].(map[string]any)
			require.Equal(t, true, harness["user"].(map[string]any)["present"])
			require.Equal(t, false, harness["overlap"])
			require.Equal(t, []any{}, report["workspace"].(map[string]any)["committed_guidance"])
			require.Contains(t, fmt.Sprint(report["warnings"]), warning)
			for _, mode := range []string{"human", "agent"} {
				out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "status", "codex", "--config", config, "--format", mode)
				require.NoError(t, err, stderr)
				require.Contains(t, out, warning)
			}
		})
	}
}
