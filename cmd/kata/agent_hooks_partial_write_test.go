package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestAgentHooksInstallPartialFailureKeepsCodexWarnings(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	root := newRootCmd()
	group, _, err := root.Find([]string{"agent-hooks"})
	require.NoError(t, err)
	original, _, err := group.Find([]string{"install"})
	require.NoError(t, err)
	group.RemoveCommand(original)
	group.AddCommand(newAgentHooksInstallCmdWithInstaller(func(agent agenthook.Agent, options agenthook.InstallOptions) (agenthook.Result, error) {
		if agent == agenthook.AgentQwen {
			return agenthook.Result{}, fmt.Errorf("fixture write failed")
		}
		return agenthook.Install(agent, options)
	}))
	out, stderr, err := executeAgentHookRoot(t, root, unreadableHookInput{}, "agent-hooks", "install", "codex", "qwen", "--executable", os.Args[0], "--json")
	require.ErrorContains(t, err, "fixture write failed")
	require.Empty(t, out)
	require.Contains(t, stderr, "earlier configs changed")
	require.Contains(t, stderr, "Codex runs new hooks only after you trust them: open Codex and run /hooks.")
	path, err := agenthook.ConfigPath(agenthook.AgentCodex)
	require.NoError(t, err)
	require.Len(t, nativeContractHandlers(t, agenthook.AgentCodex, path), 1)
}

func TestAgentHookPartialRemovalKeepsRestorationWarning(t *testing.T) {
	restoration := "workspaces initialized while the user hook existed may have no contract hook; run kata init --with-codex-hooks there to restore it"
	err := agentHookWriteError(agentHookMutation{Harness: "qwen", ConfigPath: "qwen-config"}, []agentHookMutation{{Harness: "codex", ConfigPath: "codex-config", Changed: true, Warnings: []string{codexAgentHookTrustNote, restoration}}}, fmt.Errorf("fixture removal failed"))
	require.ErrorContains(t, err, restoration)
	require.ErrorContains(t, err, codexAgentHookTrustNote)
}
