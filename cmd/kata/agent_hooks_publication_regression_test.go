package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestAgentHooksInstallThroughDotfileSymlinks(t *testing.T) {
	for _, leaf := range []bool{false, true} {
		t.Run(map[bool]string{false: "home directory", true: "config file"}[leaf], func(t *testing.T) {
			isolateAgentHookHomes(t)
			workspace := t.TempDir()
			root := t.TempDir()
			target := filepath.Join(root, "settings.json")
			require.NoError(t, os.WriteFile(target, []byte(`{"theme":"dark"}`), 0600))
			link := filepath.Join(t.TempDir(), "dotfiles")
			linkTarget, selected := root, filepath.Join(link, "settings.json")
			if leaf {
				linkTarget, selected = target, link
			}
			if err := os.Symlink(linkTarget, link); err != nil {
				t.Skip(err)
			}
			_, err := runHookSetup(t, "install", "claude", "--config", selected, "--workspace", workspace)
			require.NoError(t, err)
			entries, err := inspectAgentHookEntries(agenthook.AgentClaude, target)
			require.NoError(t, err)
			contract, start, end := kitAgentHookConfigured(agenthook.AgentClaude, entries)
			require.True(t, contract && start && end)
			_, err = runHookSetup(t, "uninstall", "claude", "--config", selected, "--workspace", workspace)
			require.NoError(t, err)
			info, err := os.Lstat(link)
			require.NoError(t, err)
			require.NotZero(t, info.Mode()&os.ModeSymlink, "preserve the dotfile manager's link")
			data, err := os.ReadFile(target) //nolint:gosec // G304: isolated dotfile target under TempDir.
			require.NoError(t, err)
			var settings map[string]any
			require.NoError(t, json.Unmarshal(data, &settings))
			require.Equal(t, "dark", settings["theme"])
			entries, err = inspectAgentHookEntries(agenthook.AgentClaude, target)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestNativeAgentHookPublicationLeavesOnlyManagedArtifacts(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "hook.js")
	plan := nativeAgentHookPlan{Changes: []nativeAgentHookChange{{Path: path, Content: []byte("managed")}}}
	changed, err := publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	require.True(t, changed)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1, "installation must not leave runtime files alongside committed hooks")
	require.Equal(t, "hook.js", entries[0].Name())
	plan.Changes[0] = nativeAgentHookChange{Path: path, Original: []byte("managed"), OriginalExists: true, Remove: true}
	changed, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	require.True(t, changed)
	entries, err = os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestPiLinkedArtifactInstallRecreatesTarget(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial dangling link", true: "reinstall after uninstall"}[installed], func(t *testing.T) {
			opts := nativeAgentHookOptions{Agent: "pi", Scope: "project", Home: t.TempDir(), Dir: t.TempDir(), Executable: "kata", Contract: true}
			initial, err := planPiAgentHooks(opts, false)
			require.NoError(t, err)
			target := filepath.Join(t.TempDir(), "managed.js")
			if installed {
				require.NoError(t, os.WriteFile(target, initial.Changes[0].Content, 0600))
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(initial.Path), 0700))
			relative, err := filepath.Rel(filepath.Dir(initial.Path), target)
			require.NoError(t, err)
			if err := os.Symlink(relative, initial.Path); err != nil {
				t.Skip(err)
			}
			if installed {
				uninstall, err := planPiAgentHooks(opts, true)
				require.NoError(t, err)
				_, err = publishNativeAgentHookPlan(uninstall)
				require.NoError(t, err)
			}
			install, err := planPiAgentHooks(opts, false)
			require.NoError(t, err)
			_, err = publishNativeAgentHookPlan(install)
			require.NoError(t, err)
			require.FileExists(t, target)
			link, err := os.Lstat(initial.Path)
			require.NoError(t, err)
			require.NotZero(t, link.Mode()&os.ModeSymlink)
			status, err := planPiAgentHooks(opts, true)
			require.NoError(t, err)
			require.True(t, status.CurrentContract)
		})
	}
}
