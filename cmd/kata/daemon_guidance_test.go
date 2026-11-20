package main

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
)

func TestProfileRecoveryCommandShells(t *testing.T) {
	for _, tc := range []struct{ os, name, want string }{
		{"windows", "work", "in PowerShell & 'kata' '--daemon' 'work' 'daemon' 'recover'"},
		{"linux", "work", "kata --daemon work daemon recover"},
		{"linux", "work #2", "kata --daemon 'work #2' daemon recover"},
	} {
		t.Run(tc.os+"/"+tc.name, func(t *testing.T) { require.Equal(t, tc.want, profileRecoveryCommand(tc.name, tc.os)) })
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
	e := &client.LocalDaemonUnreachableError{PID: 42, DataDir: t.TempDir(), WebOrigin: "http://127.0.0.1:7777", VerifiedProcess: true, Profile: "work #2"}
	unix := daemonRecoveryAdvice(e, "linux")
	require.Contains(t, unix, filepath.Join(e.DataDir, "daemon.log"))
	require.Contains(t, unix, e.WebOrigin+"/debug/pprof/goroutine?debug=2")
	require.Contains(t, unix, "kill -QUIT 42")
	require.Contains(t, unix, "kata --daemon 'work #2' daemon recover")
	require.Contains(t, daemonRecoveryAdvice(e, "windows"), "taskkill /PID 42 /F")
	e.VerifiedProcess = false
	require.NotContains(t, daemonRecoveryAdvice(e, "linux"), "kill")
	e.WebOrigin = ""
	require.NotContains(t, daemonRecoveryAdvice(e, "linux"), "/debug/pprof/")
	e.Profile = ""
	require.Contains(t, daemonRecoveryAdvice(e, "linux"), "kata daemon restart")
	require.Empty(t, daemonRecoveryAdvice(errors.New("other"), "linux"))
}

func TestDaemonRecoveryGuidanceDuringMigration(t *testing.T) {
	evidence := &client.LocalDaemonUnreachableError{PID: 42, DataDir: "/runtime", VerifiedProcess: true}
	err := errors.Join(client.ErrSQLiteMigrationActive, evidence)
	hint := daemonRecoveryAdvice(err, "linux")
	require.Contains(t, hint, "daemon.log")
	require.Contains(t, hint, "Wait")
	require.NotContains(t, hint, "kill")
	require.NotContains(t, hint, "restart")
}
