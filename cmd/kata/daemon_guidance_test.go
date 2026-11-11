package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
)

func TestProfileRecoveryCommandShells(t *testing.T) {
	for _, tc := range []struct{ os, name, want string }{
		{"windows", "work", "kata --daemon=work daemon recover"},
		{"windows", "team's work", "in PowerShell: kata '--daemon=team''s work' daemon recover"},
		{"linux", "work", "kata --daemon=work daemon recover"},
		{"linux", "=work", "kata --daemon==work daemon recover"},
		{"linux", "team's work", "kata '--daemon=team'\\''s work' daemon recover"},
	} {
		t.Run(tc.os+"/"+tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, profileRecoveryCommand(tc.name, tc.os))
		})
	}
}

func TestRawDaemonFailureOutputIncludesHint(t *testing.T) {
	err := fmt.Errorf("restart failed: %w", &client.LocalDaemonUnreachableError{PID: 42, Address: "127.0.0.1:1", DataDir: "/fixture"})
	for _, mode := range []outputMode{outputHuman, outputAgent, outputJSON} {
		var out bytes.Buffer
		emitErrorForMode(&out, err, mode, true)
		require.Contains(t, out.String(), "restart failed")
		require.Contains(t, out.String(), "daemon.log")
	}
}

func TestDaemonRecoveryAdvice(t *testing.T) {
	e := &client.LocalDaemonUnreachableError{PID: 42, DataDir: t.TempDir(), WebOrigin: "http://127.0.0.1:7777", VerifiedProcess: true}
	unix := daemonRecoveryAdvice(e, "linux")
	for _, text := range []string{filepath.Join(e.DataDir, "daemon.log"), "supervisor stderr", "If responsive", e.WebOrigin + "/debug/pprof/goroutine?debug=2", "daemon-owner access", "configured authentication use the owner's credential", "kill -QUIT 42", "terminates the daemon", "After it exits", "original foreground launch command", "auto-started", "original KATA_HOME and storage environment"} {
		require.Contains(t, unix, text)
	}
	windows := daemonRecoveryAdvice(e, "windows")
	require.Contains(t, windows, "taskkill /PID 42 /F")
	require.NotContains(t, windows, "kill -QUIT")
	e.VerifiedProcess = false
	require.Contains(t, daemonRecoveryAdvice(e, "linux"), "Verify the process identity before stopping it")
	require.NotContains(t, daemonRecoveryAdvice(e, "linux"), "kill -QUIT")
	require.NotContains(t, daemonRecoveryAdvice(e, "windows"), "taskkill")
	e.WebOrigin = ""
	require.NotContains(t, daemonRecoveryAdvice(e, "linux"), "/debug/pprof/")
}
