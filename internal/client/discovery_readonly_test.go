package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

func TestDiscoveryDoesNotCreateRuntimeDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	_, found, err := Discover(context.Background(), path)
	require.NoError(t, err)
	require.False(t, found)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "discovery must not materialize runtime state")
}

func TestDiscoveryRepairsExistingRuntimeDirectoryPrivacyWithoutChangingReadOnlyScan(t *testing.T) {
	dir := t.TempDir()
	assertUnchanged := makeRuntimeDirectoryInsecureForTest(t, dir)

	_, err := readRuntimeRecords(dir)
	require.Error(t, err, "the read-only reader must reject a runtime directory with unsafe permissions")
	assertUnchanged()

	_, found, err := Discover(context.Background(), dir)
	require.NoError(t, err, "normal discovery must preserve RuntimeStore.List's permission repair")
	require.False(t, found)
	require.NoError(t, safefileio.ValidatePrivateDir(dir), "normal discovery must repair the runtime directory")
}

func TestReadRuntimeRecordsRequiresExactFieldNames(t *testing.T) {
	dir, err := os.MkdirTemp(t.TempDir(), "runtime")
	require.NoError(t, err)
	require.NoError(t, safefileio.EnsurePrivateDir(dir))
	path := filepath.Join(dir, "daemon.123.json")
	body := []byte(`{"PID":123,"Address":"http://daemon.example"}`)
	require.NoError(t, os.WriteFile(path, body, 0600))

	records, err := readRuntimeRecords(dir)
	require.NoError(t, err)
	require.Empty(t, records, "runtime records use the exact field names written by the daemon")
}

func TestReadRuntimeRecordsFindsKitRuntimeStoreRecord(t *testing.T) {
	dir := t.TempDir()
	store := kitdaemon.RuntimeStore{Dir: dir}
	written, err := store.Write(kitdaemon.NewRuntimeRecord("kata", "test", kitdaemon.Endpoint{
		Network: "tcp", Address: "127.0.0.1:7777",
	}))
	require.NoError(t, err)

	records, err := readRuntimeRecords(dir)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, os.Getpid(), records[0].PID)
	require.Equal(t, "127.0.0.1:7777", records[0].Address)
	require.Equal(t, written, records[0].SourcePath)
}
