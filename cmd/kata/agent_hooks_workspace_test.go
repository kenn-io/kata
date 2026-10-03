package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestAgentHooksStatusDiscoversWorkspaceFromSubdirectory(t *testing.T) {
	isolateAgentHookHomes(t)
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.name", "Example Actor")
	runGit(t, repo, "config", "user.email", "actor@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(agentsBlockBegin+"\nfixture\n"+agentsBlockEnd), 0o600))
	runGit(t, repo, "add", "AGENTS.md")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
	workspaceConfig := filepath.Join(repo, ".codex", "hooks.json")
	installHookFixture(t, agenthook.AgentCodex, workspaceConfig, "kata", legacyAgentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	userConfig, err := agenthook.ConfigPath(agenthook.AgentCodex)
	require.NoError(t, err)
	installHookFixture(t, agenthook.AgentCodex, userConfig, "kata", legacyAgentContractHookSource, agenthook.Hook{Event: agenthook.EventSessionStart})
	subdirectory := filepath.Join(repo, "src")
	require.NoError(t, os.Mkdir(subdirectory, 0o700))
	t.Chdir(subdirectory)
	report := agentHooksStatusJSON(t, "codex")
	require.Equal(t, repo, report["workspace"].(map[string]any)["path"])
	harness := report["harnesses"].([]any)[0].(map[string]any)
	require.Equal(t, true, harness["duplicate"])
	require.Equal(t, true, harness["overlap"])
	// An explicit workspace denotes exactly that directory, even within a repo.
	report = agentHooksStatusJSON(t, "codex", "--workspace", subdirectory)
	require.Equal(t, subdirectory, report["workspace"].(map[string]any)["path"])
	require.Equal(t, false, report["harnesses"].([]any)[0].(map[string]any)["duplicate"])
}

func TestAgentHooksStatusReadsCommittedGuidanceFromAliasedWorkspace(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.name", "Example Actor")
	runGit(t, repo, "config", "user.email", "actor@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(agentsBlockBegin+"\nfixture\n"+agentsBlockEnd), 0o600))
	runGit(t, repo, "add", "AGENTS.md")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")

	alias := filepath.Join(t.TempDir(), "workspace-alias")
	require.NoError(t, os.Symlink(repo, alias))

	got, err := readCommittedGuidance(alias)
	require.NoError(t, err)
	require.Equal(t, []string{"AGENTS.md"}, got)
}
