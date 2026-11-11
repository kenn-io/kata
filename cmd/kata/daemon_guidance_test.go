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
		{"windows", "work", "in PowerShell & 'kata' '--daemon=work' 'daemon' 'recover'"},
		{"windows", "team's work", "in PowerShell & 'kata' '--daemon=team''s work' 'daemon' 'recover'"},
		{"linux", "work", "kata --daemon=work daemon recover"},
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
	e := &client.LocalDaemonUnreachableError{PID: 42, DataDir: filepath.Join(t.TempDir(), `space "quote`), WebOrigin: "http://127.0.0.1:7777", VerifiedProcess: true}
	unix := daemonRecoveryAdvice(e, "", "linux")
	require.Contains(t, unix, "This probe failed")
	require.NotContains(t, unix, ";")
	require.NotContains(t, unix, "only if")
	for _, text := range []string{fmt.Sprintf("read %q, if present, or the daemon's retained stderr", filepath.Join(e.DataDir, "daemon.log")), "If " + e.WebOrigin + " responds, capture " + e.WebOrigin + "/debug/pprof/goroutine?debug=2 with daemon-owner access, using the owner's credential when authentication is configured", "If you choose to stop it, immediately recheck that PID 42 is still this daemon and the selected endpoint still fails", "Then kill -QUIT 42 stops it and writes goroutine stacks to its stderr, which are lost if stderr is discarded", "After it exits, restart a supervised or foreground daemon through its supervisor or original command", "auto-started", "original KATA_HOME and storage environment"} {
		require.Contains(t, unix, text)
	}
	windows := daemonRecoveryAdvice(e, "", "windows")
	require.Contains(t, windows, "Then taskkill /PID 42 /F stops it")
	require.NotContains(t, windows, "kill -QUIT")
	e.VerifiedProcess = false
	require.NotContains(t, daemonRecoveryAdvice(e, "", "linux"), "kill -QUIT")
	require.NotContains(t, daemonRecoveryAdvice(e, "", "windows"), "taskkill")
	e.WebOrigin = ""
	require.NotContains(t, daemonRecoveryAdvice(e, "", "linux"), "/debug/pprof/")
}
