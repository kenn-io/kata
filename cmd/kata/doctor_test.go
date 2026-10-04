package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/diagnostics"
)

func doctorTestEnv(t *testing.T) (string, string) {
	t.Helper()
	resetFlags(t)
	home, workspace := t.TempDir(), t.TempDir()
	for _, key := range []string{"KATA_DSN", "KATA_SERVER", "KATA_AUTH_TOKEN", "KATA_TRUST_PRIVATE_NETWORK", "KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES", "PORT"} {
		t.Setenv(key, "")
	}
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", filepath.Join(home, "kata.db"))
	return home, workspace
}

func TestDoctorOutputModesAndQuiet(t *testing.T) {
	report := diagnostics.NewReport([]diagnostics.Check{
		{ID: "daemon.connection", Category: "daemon", Status: "ok", Summary: "reachable"},
		{ID: "daemon.hooks", Category: "hooks", Status: "warn", Summary: "check hooks", Details: []string{"command\nwith newline"}, Fix: "inspect logs"},
	})
	for _, mode := range []outputMode{outputHuman, outputAgent, outputJSON} {
		var out bytes.Buffer
		require.NoError(t, writeDoctorReport(&out, report, mode, true))
		if mode == outputJSON {
			var got diagnostics.Report
			require.NoError(t, json.Unmarshal(out.Bytes(), &got))
			require.Len(t, got.Checks, 2, "JSON remains complete in quiet mode")
			require.Equal(t, report.Summary, got.Summary)
		} else {
			require.NotContains(t, out.String(), "daemon.connection")
			require.Contains(t, out.String(), "daemon.hooks")
			require.Contains(t, out.String(), `command\nwith newline`)
			require.Contains(t, out.String(), "1 warn, 0 fail")
		}
	}
}

func TestDoctorAgentOutputIncludesDetailsWithFinding(t *testing.T) {
	report := diagnostics.NewReport([]diagnostics.Check{{
		ID: "daemon.hook_runs", Category: "hooks", Status: "warn", Summary: "hook failures retained",
		Details: []string{"retained failures: 3", "history incomplete"},
	}})
	var out bytes.Buffer
	require.NoError(t, writeDoctorReport(&out, report, outputAgent, false))

	line, _, ok := strings.Cut(out.String(), "\n")
	require.True(t, ok, "agent output contains a finding and summary")
	require.Contains(t, line, `detail="retained failures: 3"`)
	require.Contains(t, line, `detail="history incomplete"`)
}

func TestDoctorHumanDetailsRemainReadable(t *testing.T) {
	report := diagnostics.NewReport([]diagnostics.Check{{
		ID: "daemon.hooks", Status: "fail",
		Details: []string{"hook 0 command \"missing-hook\" is unavailable\ncheck PATH"},
	}})
	var out bytes.Buffer
	require.NoError(t, writeDoctorReport(&out, report, outputHuman, false))
	require.Contains(t, out.String(), "  hook 0 command \"missing-hook\" is unavailable\\ncheck PATH\n")
}

func TestDoctorFailedChecksHaveTheirOwnErrorKind(t *testing.T) {
	for _, mode := range []string{"--agent", "--json"} {
		t.Run(mode, func(t *testing.T) {
			_, workspace := doctorTestEnv(t)
			_, stderr, err := executeRootCapture(t, context.Background(), "doctor", "--workspace", workspace, mode)
			require.Error(t, err)
			require.Equal(t, 1, exitCodeForErr(err, true))
			if mode == "--agent" {
				require.Contains(t, stderr, "ERR doctor checks_failed:")
			} else {
				var envelope struct {
					Error struct {
						Kind string `json:"kind"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(stderr), &envelope))
				require.Equal(t, "checks_failed", envelope.Error.Kind)
			}
		})
	}
}

type doctorFailingWriter struct{}

func (doctorFailingWriter) Write([]byte) (int, error) { return 0, errors.New("output closed") }

func TestDoctorPropagatesOutputErrors(t *testing.T) {
	report := diagnostics.NewReport([]diagnostics.Check{{ID: "example", Status: "ok"}})
	for _, mode := range []outputMode{outputHuman, outputAgent, outputJSON} {
		require.ErrorContains(t, writeDoctorReport(doctorFailingWriter{}, report, mode, false), "output closed")
	}
}

func TestDoctorWarningsExitSuccessfullyAndInvalidModesKeepCLIError(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	ctx := doctorServerContext(t, nil, nil)
	_, _, err := executeRootCapture(t, ctx, "doctor", "--workspace", workspace, "--json")
	require.NoError(t, err, "unbound workspace and unavailable hook endpoint only warn")
	_, _, err = executeRootCapture(t, ctx, "doctor", "--json", "--agent")
	require.Error(t, err)
	require.Equal(t, ExitUsage, exitCodeForErr(err, false))
}

func TestDoctorStoppedDaemonReportsWithoutCreatingFiles(t *testing.T) {
	home, workspace := doctorTestEnv(t)
	out, _, err := executeRootCapture(t, context.Background(), "doctor", "--workspace", workspace, "--json")
	require.Error(t, err)
	require.Equal(t, 1, exitCodeForErr(err, true))
	var report struct {
		Version int `json:"version"`
		Checks  []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report), out)
	require.Equal(t, 1, report.Version)
	var found bool
	for _, check := range report.Checks {
		if check.ID == "daemon.connection" {
			found = true
			require.EqualValues(t, "fail", check.Status)
		}
	}
	require.True(t, found)
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Empty(t, entries, "doctor must not create DB, runtime, logs, or config")
}

func TestDoctorDoesNotRepairRuntimeDirectoryPermissions(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	assertUnchanged := makeDoctorRuntimeDirectoryInsecureForTest(t, ns.DataDir)

	out, _, err := executeRootCapture(t, context.Background(), "doctor", "--workspace", workspace, "--json")
	require.Error(t, err)
	var report struct {
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report), out)
	var found bool
	for _, check := range report.Checks {
		if check.ID == "daemon.connection" {
			found = true
			require.EqualValues(t, "fail", check.Status)
		}
	}
	require.True(t, found, "doctor must report the unavailable local runtime")
	assertUnchanged()
}
