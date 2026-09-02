//go:build windows

package connector

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"golang.org/x/sys/windows"
)

func TestProcessClientChildCanUseWindowsNativeTempAndProfile(t *testing.T) {
	t.Setenv("TEMP", t.TempDir())
	t.Setenv("TMP", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("HELPER_MODE", "native-runtime-paths")
	client := newHelperClient(t, config.ConnectorConfig{
		ID: "notes", Command: helperBinary(t), Args: []string{"-test.run=^TestProcessClientHelper$"},
		Env: map[string]string{"MODE": "HELPER_MODE"},
	})

	_, err := client.Describe(t.Context())
	require.NoError(t, err)
}

type processClientHelperObservation struct {
	process *os.Process
}

func observeProcessClientHelper(t *testing.T, pid int) processClientHelperObservation {
	t.Helper()
	process, err := os.FindProcess(pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return processClientHelperObservation{}
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = process.Release() })
	return processClientHelperObservation{process: process}
}

func requireProcessClientHelperGone(t *testing.T, observed processClientHelperObservation) {
	t.Helper()
	if observed.process == nil {
		return
	}
	var waitResult uint32
	var waitErr error
	require.NoError(t, observed.process.WithHandle(func(handle uintptr) {
		waitResult, waitErr = windows.WaitForSingleObject(windows.Handle(handle), 2_000)
	}))
	require.NoError(t, waitErr)
	require.Equal(t, uint32(windows.WAIT_OBJECT_0), waitResult, "connector descendant survived cancellation")
}
