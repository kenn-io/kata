package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestAgentHooksInstallReplacesOwnedBehaviorFields(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	for _, field := range []string{"args", "if"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			planned, err := agenthook.PlanInstall(agenthook.AgentClaude, agenthook.InstallOptions{
				ConfigPath: path, Executable: os.Args[0], Arguments: []string{"agent-hooks", "contract", "claude", "--source", agentContractHookSource},
				Marker: agentContractMarker, Hooks: []agenthook.Hook{contractRegistrationHook(agenthook.AgentClaude)},
			})
			require.NoError(t, err)
			var document map[string]any
			require.NoError(t, json.Unmarshal(planned.Data, &document))
			groups := document["hooks"].(map[string]any)["SessionStart"].([]any)
			handler := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
			if field == "args" {
				handler[field] = []any{}
			} else {
				handler[field] = "Bash(git *)"
			}
			foreign := map[string]any{"type": "command", "command": "example foreign", "args": []any{"keep"}, "if": "Bash(git *)"}
			groups[0].(map[string]any)["hooks"] = []any{foreign, handler}
			data, err := json.Marshal(document)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o600))
			out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "claude", "--config", path, "--executable", os.Args[0])
			require.NoError(t, err, stderr)
			require.Contains(t, out, "installed")
			require.Contains(t, out, "re-trust")
			document = nativeHookDocument(t, agenthook.AgentClaude, path)
			groups = document["hooks"].(map[string]any)["SessionStart"].([]any)
			require.Len(t, groups, 2)
			require.Equal(t, foreign, groups[0].(map[string]any)["hooks"].([]any)[0])
			owned := groups[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)
			require.NotContains(t, owned, field)
		})
	}
}
