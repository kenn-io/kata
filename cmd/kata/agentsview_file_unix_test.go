//go:build unix

package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Explicit contract: a snapshot/consent must be regular and bounded. Opening a
// FIFO to inspect it must not wait for another process or evade cancellation.
func TestAgentsViewSelectedFIFOCannotBlockCaptureOrConsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-file")
	require.NoError(t, unix.Mkfifo(path, 0o600))
	snapshot, err := captureAgentsViewSnapshot(t.Context(), path)
	require.Error(t, err)
	require.Nil(t, snapshot)
	_, err = readAgentsViewConsent(t.Context(), path)
	require.Error(t, err)
}
