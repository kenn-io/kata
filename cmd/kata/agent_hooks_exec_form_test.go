package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

// Kit matches its removal marker against Claude command and args joined, so a
// foreign hook whose arguments contain the marker must survive Kata's install.
func TestAgentHooksInstallKeepsForeignClaudeArgsContainingMarker(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "settings.json")
	foreign := map[string]any{"type": "command", "command": "example-tool", "args": []any{"contract"}}
	document := map[string]any{"hooks": map[string]any{"SessionStart": []any{
		map[string]any{"hooks": []any{foreign}},
	}}}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "install", "--contract-only", "claude", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	var kept []any
	for _, handler := range nativeContractHandlers(t, agenthook.AgentClaude, path) {
		if handler["command"] == "example-tool" {
			kept = append(kept, handler["args"])
		}
	}
	require.Equal(t, []any{[]any{"contract"}}, kept)
}

// Kit writes Claude hooks on Windows as an executable plus separate args.
func TestClassifyClaudeExecFormHandlers(t *testing.T) {
	handler := func(command string, args ...any) map[string]any {
		return map[string]any{"type": "command", "command": command, "args": args}
	}
	for name, tc := range map[string]struct {
		handler map[string]any
		want    agentHookKind
	}{
		"contract":           {handler(`C:\Program Files\kata\kata.exe`, "agent-hook", "contract", "claude"), contractHook},
		"attention start":    {handler(`C:\Program Files\kata\kata.exe`, "agent-hook", "attention-native", "claude", "start"), attentionStartHook},
		"init attention end": {handler("kata", "attention-hook", "end"), attentionEndHook},
		"renamed executable": {handler(`C:\tools\kata-dev.exe`, "agent-hook", "contract", "claude", "--source", legacyAgentContractHookSource), contractHook},
		"renamed unmarked":   {handler(`C:\tools\kata-dev.exe`, "agent-hook", "contract", "claude"), ""},
		"foreign":            {handler("example-tool", "contract"), ""},
		"extra argument":     {handler("kata", "agent-hook", "contract", "claude", "--verbose"), ""},
		"non-string arg":     {handler("kata", "agent-hook", "contract", 1), ""},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, classifyAgentHookHandler(agenthook.AgentClaude, tc.handler))
		})
	}
}

func TestAgentHookStatusReadsClaudeExecForm(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Program Files")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	executable := filepath.Join(dir, "kata")
	require.NoError(t, os.WriteFile(executable, nil, 0o600))
	path := filepath.Join(t.TempDir(), "settings.json")
	handler := map[string]any{"type": "command", "command": executable, "args": []any{"agent-hook", "contract", "claude"}}
	data, err := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": []any{
		map[string]any{"matcher": "startup|resume|clear|compact", "hooks": []any{handler}},
	}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	status, err := readAgentHookStatus(agenthook.AgentClaude, path, t.TempDir())
	require.NoError(t, err)
	require.Len(t, status.Entries, 1)
	entry := status.Entries[0]
	require.Equal(t, executable, entry.Executable)
	require.True(t, entry.ExecutableExists)
	commands, err := agenthook.BuildCommand(executable, "agent-hook", "contract", "claude")
	require.NoError(t, err)
	require.Equal(t, commands.Native, entry.Command)
}

func TestEffectiveAgentHookDefaultAcceptsClaudeExecForm(t *testing.T) {
	handler := map[string]any{"type": "command", "command": `C:\kata\kata.exe`, "args": []any{"agent-hook", "contract", "claude"}, "timeout": 10}
	entry := makeAgentHookEntry(agenthook.AgentClaude, "SessionStart", 0, 0, map[string]any{}, handler)
	require.True(t, effectiveAgentHookDefault(agenthook.AgentClaude, entry))
}
