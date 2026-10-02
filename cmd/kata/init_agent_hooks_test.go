package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestInitNativeProjectHookBundle(t *testing.T) {
	isolateAgentHookHomes(t)
	env, dir, _ := setupCLIWorkspace(t)
	runCLI(t, env, dir, "init", "--with-agent-hooks", "codex", "--with-agent-hooks", "pi")
	entries, err := inspectAgentHookEntries(agenthook.AgentCodex, filepath.Join(dir, ".codex", "hooks.json"))
	require.NoError(t, err)
	contract, start, end := kitAgentHookConfigured(agenthook.AgentCodex, entries)
	require.True(t, contract)
	require.True(t, start)
	require.True(t, end)
	data, err := os.ReadFile(filepath.Join(dir, ".pi", "extensions", "kata.js")) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	require.NoError(t, err)
	meta, err := parsePiAgentHookMetadata(data)
	require.NoError(t, err)
	require.Equal(t, "project", meta.Scope)
	require.True(t, meta.Attention)
}

func TestInitNativeProjectHookRejectsUnsupportedBeforeWrites(t *testing.T) {
	home := isolateAgentHookHomes(t)
	dir := filepath.Join(home, "new-workspace")
	require.NoError(t, os.MkdirAll(dir, 0700))
	resetFlags(t)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "init", "--with-agent-hooks", "hermes", "--workspace", dir)
	require.ErrorContains(t, err, "no verified project")
	_, err = os.Stat(filepath.Join(dir, ".kata.toml"))
	require.True(t, os.IsNotExist(err))
}
