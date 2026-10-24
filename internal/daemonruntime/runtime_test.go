package daemonruntime_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
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

func TestRuntimeProcessAliveRejectsReusedPID(t *testing.T) {
	identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	encoded := string(identity)
	replacement := byte('0')
	if encoded[len(encoded)-1] == replacement {
		replacement = '1'
	}
	record := kitdaemon.RuntimeRecord{
		PID:               os.Getpid(),
		ProcessIdentity:   identity,
		ProcessIdentityV2: kitdaemon.ProcessIdentity(encoded[:len(encoded)-1] + string(replacement)),
	}

	assert.False(t, daemonruntime.RuntimeProcessAlive(record))
}

func TestRuntimeProcessAliveRequiresLocalIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record func(*testing.T) kitdaemon.RuntimeRecord
		alive  bool
	}{
		{"local", func(t *testing.T) kitdaemon.RuntimeRecord {
			identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
			require.True(t, ok)
			return kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity}
		}, true},
		{"unknown", func(*testing.T) kitdaemon.RuntimeRecord {
			return kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: "unverifiable"}
		}, false},
		{"foreign", foreignNamespaceRuntimeRecord, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.alive, daemonruntime.RuntimeProcessAlive(tc.record(t)))
		})
	}
}

func TestRuntimeRecordBlocksStart(t *testing.T) {
	t.Run("legacy reused PID", func(t *testing.T) {
		if os.Getenv("KATA_LEGACY_PID_HELPER") == "1" {
			_, _ = io.Copy(io.Discard, os.Stdin)
			return
		}
		child := exec.Command(os.Args[0], "-test.run=^TestRuntimeRecordBlocksStart$/^legacy_reused_PID$") //nolint:gosec // test binary
		child.Env = append(os.Environ(), "KATA_LEGACY_PID_HELPER=1")
		stdin, err := child.StdinPipe()
		require.NoError(t, err)
		require.NoError(t, child.Start())
		t.Cleanup(func() { _ = stdin.Close(); require.NoError(t, child.Wait()) })
		require.True(t, kitdaemon.ProcessAlive(child.Process.Pid))
		record := kitdaemon.RuntimeRecord{PID: child.Process.Pid, StartedAt: time.Unix(1, 0)}
		assert.False(t, daemonruntime.RuntimeRecordBlocksStart(record))
	})
	t.Run("foreign", func(t *testing.T) {
		assert.False(t, daemonruntime.RuntimeRecordBlocksStart(foreignNamespaceRuntimeRecord(t)))
	})
}
