package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func runHookSetup(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetFlags(t)
	out, _, err := executeAgentHook(t, unreadableHookInput{}, append([]string{"agent-hooks"}, args...)...)
	return out, err
}

func requireCodexComponents(t *testing.T, path string, contract, start, end bool) {
	t.Helper()
	entries, err := inspectAgentHookEntries(agenthook.AgentCodex, path)
	require.NoError(t, err)
	gotContract, gotStart, gotEnd := kitAgentHookConfigured(agenthook.AgentCodex, entries)
	require.Equal(t, contract, gotContract, "contract")
	require.Equal(t, start, gotStart, "attention start")
	require.Equal(t, end, gotEnd, "attention end")
}

func TestAgentHooksSetupDefaultsAndComponentSelection(t *testing.T) {
	for _, selection := range []string{"default", "--contract-only", "--attention=false", "--attention"} {
		t.Run(selection, func(t *testing.T) {
			isolateAgentHookHomes(t)
			dir := t.TempDir()
			args := []string{"install", "codex", "--scope", "project", "--workspace", dir, "--executable", os.Args[0]}
			if selection != "default" {
				args = append(args, selection)
			}
			_, err := runHookSetup(t, args...)
			require.NoError(t, err)
			full := selection == "default" || selection == "--attention"
			path := filepath.Join(dir, ".codex", "hooks.json")
			requireCodexComponents(t, path, true, full, full)
			_, err = runHookSetup(t, "install", "codex", "--scope", "project", "--workspace", dir, "--executable", os.Args[0], "--contract-only")
			require.NoError(t, err)
			requireCodexComponents(t, path, true, full, full)
			_, err = runHookSetup(t, "uninstall", "codex", "--scope", "project", "--workspace", dir, "--contract-only")
			require.NoError(t, err)
			requireCodexComponents(t, path, false, full, full)
			_, err = runHookSetup(t, "uninstall", "codex", "--scope", "project", "--workspace", dir)
			require.NoError(t, err)
			requireCodexComponents(t, path, false, false, false)
		})
	}
}

func TestAgentHooksSetupLocalAndConflictsBeforeWrites(t *testing.T) {
	for _, args := range [][]string{{"--local", "--scope", "user"}, {"--contract-only", "--attention"}, {"--contract-only", "--managed-attention"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			dir := t.TempDir()
			_, err := runHookSetup(t, append([]string{"install", "codex", "--workspace", dir, "--executable", os.Args[0]}, args...)...)
			require.Error(t, err)
			require.NoDirExists(t, filepath.Join(dir, ".codex"))
			require.NoDirExists(t, filepath.Join(home, "codex"))
		})
	}
	isolateAgentHookHomes(t)
	dir := t.TempDir()
	_, err := runHookSetup(t, "install", "codex", "--local", "--workspace", dir, "--executable", os.Args[0])
	require.NoError(t, err)
	requireCodexComponents(t, filepath.Join(dir, ".codex", "hooks.json"), true, true, true)
}

func TestAgentHooksSetupDiscoveryUsesAgentSignals(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "local"}[local], func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(home, "codex"), 0700))
			for _, generic := range []string{".github", ".agents"} {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, generic), 0700))
			}
			args := []string{"install", "--workspace", dir, "--executable", os.Args[0], "--json"}
			if local {
				args = append(args, "--local")
			}
			out, err := runHookSetup(t, args...)
			require.NoError(t, err)
			var report struct {
				Results []agentHookMutation `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &report))
			installed := []string{}
			for _, result := range report.Results {
				if result.Changed {
					installed = append(installed, result.Harness)
				}
			}
			require.Equal(t, []string{"codex"}, installed)
			path := filepath.Join(home, "codex", "hooks.json")
			if local {
				path = filepath.Join(dir, ".codex", "hooks.json")
			}
			requireCodexComponents(t, path, true, true, true)
			require.NoFileExists(t, filepath.Join(dir, ".github", "hooks", "kata.json"))
			require.NoFileExists(t, filepath.Join(dir, ".agents", "hooks.json"))
		})
	}
}

func TestAgentHooksSetupExplicitTargetsRemainDeterministic(t *testing.T) {
	home := isolateAgentHookHomes(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "claude"), 0700))
	_, err := runHookSetup(t, "install", "codex", "--executable", os.Args[0])
	require.NoError(t, err)
	requireCodexComponents(t, filepath.Join(home, "codex", "hooks.json"), true, true, true)
	require.NoFileExists(t, filepath.Join(home, "claude", "settings.json"))
}

func TestAgentHooksSetupBareStatusBothScopes(t *testing.T) {
	isolateAgentHookHomes(t)
	dir := t.TempDir()
	_, err := runHookSetup(t, "install", "codex", "--local", "--workspace", dir, "--executable", os.Args[0])
	require.NoError(t, err)
	out, err := runHookSetup(t, "status", "--workspace", dir, "--json")
	require.NoError(t, err)
	var report agentHookStatusReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	found := map[string]bool{}
	for _, row := range slices.Concat(report.Harnesses, report.Workspace.Harnesses) {
		if row.Harness == "codex" {
			found[row.Scope] = row.Configured.Contract
		}
	}
	require.Equal(t, map[string]bool{"user": false, "project": true}, found)
	out, err = runHookSetup(t, "status", "codex", "--workspace", dir, "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Harnesses, 1)
	require.Equal(t, "user", report.Harnesses[0].Scope)
}
