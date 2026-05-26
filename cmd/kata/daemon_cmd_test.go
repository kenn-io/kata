package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/testenv"
)

func TestDaemonStatus_NoDaemonReportsAbsent(t *testing.T) {
	setupKataEnv(t)

	out := executeRoot(t, newDaemonCmd(), "status")
	assert.Contains(t, string(out), "no daemon")
}

func TestDaemonStatus_JSONReportsDaemonsWithVersion(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	started := time.Date(2026, 5, 4, 1, 2, 3, 0, time.UTC)
	_, err = daemon.WriteRuntimeFile(ns.DataDir, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Address:   "unix:///tmp/kata-test.sock",
		DBPath:    filepath.Join(tmp, "kata.db"),
		Version:   "v-test-status",
		StartedAt: started,
	})
	require.NoError(t, err)

	out := executeRoot(t, newRootCmd(), "daemon", "status", "--json")

	var got struct {
		KataAPIVersion int `json:"kata_api_version"`
		Daemons        []struct {
			PID       int    `json:"pid"`
			Version   string `json:"version"`
			Address   string `json:"address"`
			DBPath    string `json:"db_path"`
			StartedAt string `json:"started_at"`
		} `json:"daemons"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, 1, got.KataAPIVersion)
	require.Len(t, got.Daemons, 1)
	assert.Equal(t, os.Getpid(), got.Daemons[0].PID)
	assert.Equal(t, "v-test-status", got.Daemons[0].Version)
	assert.Equal(t, "unix:///tmp/kata-test.sock", got.Daemons[0].Address)
	assert.Equal(t, filepath.Join(tmp, "kata.db"), got.Daemons[0].DBPath)
	assert.Equal(t, started.Format(time.RFC3339), got.Daemons[0].StartedAt)
}

func TestDaemonStatus_JSONReportsEmptyDaemonList(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	out := executeRoot(t, newRootCmd(), "daemon", "status", "--json")

	var got struct {
		KataAPIVersion int             `json:"kata_api_version"`
		Daemons        json.RawMessage `json:"daemons"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	assert.JSONEq(t, "[]", string(got.Daemons))
}

func TestDaemonStatus_AgentReportsStopped(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	out := executeRoot(t, newRootCmd(), "--agent", "daemon", "status")
	assert.Equal(t, "OK daemon status=stopped\n", string(out))
}

func TestHealth_AgentReportsOK(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	cmd := newRootCmd()
	cmd.SetContext(contextWithBaseURL(context.Background(), env.URL))

	out := executeRoot(t, cmd, "--agent", "health")
	assert.Contains(t, string(out), "OK health")
}

func TestDaemonStart_ListenFlagRejectsPublicAddress(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"daemon", "start", "--listen", "8.8.8.8:7777"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-public")
}

func TestDaemonStart_ListenFlagRejectsMalformed(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"daemon", "start", "--listen", "not-a-host-port"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--listen")
}

// TestDaemonStart_ConfigFileListenIsHonored verifies that
// <KATA_HOME>/config.toml's `listen = ...` value is picked up when the
// --listen flag is absent. We use an obviously-public address so the
// validator rejects it before the daemon actually starts — this lets us
// assert that the config value was consulted (otherwise the daemon would
// fall through to the Unix-socket path and not error).
func TestDaemonStart_ConfigFileListenIsHonored(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "config.toml"),
		[]byte(`listen = "8.8.8.8:7777"`+"\n"), 0o600))

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"daemon", "start"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-public",
		"config.toml listen value must reach the validator")
}

// TestDaemonStart_FlagWinsOverConfigFile asserts the --listen flag
// takes precedence over <KATA_HOME>/config.toml.
func TestDaemonStart_FlagWinsOverConfigFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))
	// Config file says one thing, flag says another — flag must win.
	// Both are public so the daemon will reject either, but only the
	// flag's address should appear in the error.
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "config.toml"),
		[]byte(`listen = "1.1.1.1:7777"`+"\n"), 0o600))

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"daemon", "start", "--listen", "8.8.8.8:7777"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "8.8.8.8")
	assert.NotContains(t, err.Error(), "1.1.1.1",
		"config.toml value must NOT win when --listen is set")
}

func TestEnsureDaemon_ReturnsExistingURL(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	tmp := setupKataEnv(t)

	addr, cleanup := pipeServer(t)
	t.Cleanup(cleanup)
	require.NoError(t, writeRuntimeFor(tmp, addr))

	url, err := ensureDaemon(context.Background())
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(url, "http://"))
}
