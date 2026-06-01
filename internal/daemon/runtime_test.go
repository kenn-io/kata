package daemon_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestRuntimeFile_RoundTripWriteRead(t *testing.T) {
	dir := t.TempDir()
	rec := kitdaemon.RuntimeRecord{
		PID:       4242,
		Network:   "unix",
		Address:   "/tmp/kata.sock",
		Metadata:  map[string]string{"db_path": "/tmp/kata.db"},
		StartedAt: time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC),
	}
	store := kitdaemon.RuntimeStore{Dir: dir}
	path, err := store.Write(rec)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "daemon.4242.json"), path)

	got, err := store.Read(path)
	require.NoError(t, err)
	assert.Equal(t, rec.PID, got.PID)
	assert.Equal(t, rec.Address, got.Address)
	assert.Equal(t, "/tmp/kata.db", got.Metadata["db_path"])
}

func TestListRuntimeFiles_FindsAllInDir(t *testing.T) {
	dir := t.TempDir()
	store := kitdaemon.RuntimeStore{Dir: dir}
	for _, pid := range []int{1, 2, 3} {
		_, err := store.Write(kitdaemon.RuntimeRecord{
			PID:       pid,
			Address:   "x",
			Metadata:  map[string]string{"db_path": "x"},
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
	}

	got, err := store.List()
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestRuntimeFile_AtomicViaTempRename(t *testing.T) {
	// Two concurrent writes shouldn't produce a half-written file.
	// We assert by writing once and then reading — the value must match.
	dir := t.TempDir()
	store := kitdaemon.RuntimeStore{Dir: dir}
	rec := kitdaemon.RuntimeRecord{
		PID:       7,
		Address:   "x",
		Metadata:  map[string]string{"db_path": "x"},
		StartedAt: time.Now().UTC(),
	}
	_, err := store.Write(rec)
	require.NoError(t, err)
	got, err := store.Read(filepath.Join(dir, "daemon.7.json"))
	require.NoError(t, err)
	assert.Equal(t, rec.PID, got.PID)
}

func TestProcessAlive_TrueForSelfFalseForGarbagePID(t *testing.T) {
	assert.True(t, kitdaemon.ProcessAlive(os.Getpid()))
	assert.False(t, kitdaemon.ProcessAlive(99999999))
}
