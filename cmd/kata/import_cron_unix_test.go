//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestImportForceDoesNotReplaceNewlyAppearedDestination(t *testing.T) {
	home, input, path := setupImportTest(t)
	//nolint:gosec // Input is an owned temporary import fixture.
	snapshot, err := os.ReadFile(input)
	require.NoError(t, err)
	fifo := filepath.Join(home, "input.fifo")
	require.NoError(t, unix.Mkfifo(fifo, 0600))
	done := make(chan error, 1)
	go func() {
		_, err := runCmdOutput(t, nil, "import", "--force", "--input", fifo, "--target", path)
		done <- err
	}()
	// Opening the FIFO writer rendezvous with the command's input open, which
	// happens after it classified the destination as missing. Withhold input
	// until another writer has created the destination: no timing sleeps/hooks.
	//nolint:gosec // FIFO is an owned temporary fixture, not an external path.
	writer, err := os.OpenFile(fifo, os.O_WRONLY, 0600)
	require.NoError(t, err)
	concurrent := openKataTestDB(t, path)
	project, err := concurrent.CreateProject(t.Context(), "newly-created-project")
	require.NoError(t, err)
	expectedUID := concurrent.InstanceUID()
	require.NoError(t, concurrent.Close())
	_, err = writer.Write(snapshot)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	select {
	case err := <-done:
		require.Error(t, err, "--force must not authorize overwriting a target that appeared after classification")
	case <-time.After(30 * time.Second):
		t.Fatal("import did not finish")
	}
	retained := openKataTestDB(t, path)
	defer func() { _ = retained.Close() }()
	require.Equal(t, expectedUID, retained.InstanceUID())
	_, err = retained.ProjectByUID(t.Context(), project.UID)
	require.NoError(t, err)
}
