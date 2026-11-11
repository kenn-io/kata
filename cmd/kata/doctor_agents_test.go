package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDoctorAgentProbeDeadline(t *testing.T) {
	isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "hang")
	opts, _, err := agentSetupOptions("codex")
	require.NoError(t, err)
	plan, err := planNativeAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	start := time.Now()
	check := doctorAgent(t.Context(), "codex")
	require.Less(t, time.Since(start), 8*time.Second)
	require.Contains(t, check.Summary, "deadline exceeded")
}

func TestDoctorClaudeUsesIsolatedProfile(t *testing.T) {
	require.Equal(t, filepath.Join(os.Getenv("CODEX_HOME"), "claude"), os.Getenv("CLAUDE_CONFIG_DIR"))
	home := isolateAgentHookHomes(t)
	dir := t.TempDir()
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0700)) //nolint:gosec // The fixture must be executable.
	t.Setenv("PATH", dir)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(`{"disableAllHooks":true}`), 0600))
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.Error(t, err)
	resetFlags(t)
	report := collectDoctor(t.Context())
	check := doctorFinding(t, report, "agent.claude")
	require.Equal(t, "warn", string(check.Status))
	require.Contains(t, check.Fix, "disableAllHooks")
	codex := doctorFinding(t, report, "agent.codex")
	require.Equal(t, "info", string(codex.Status))
}

func TestDoctorAgentMissingWithHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Setenv("PATH", t.TempDir())
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hook contract claude"}]}]}}`), 0600))
	check := doctorAgent(t.Context(), "claude")
	require.Equal(t, "warn", string(check.Status))
	require.Contains(t, check.Summary, "unknown")
	require.Contains(t, check.Fix, "PATH")
}
