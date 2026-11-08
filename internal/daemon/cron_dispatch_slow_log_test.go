//go:build linux

package daemon_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// A stalled diagnostic destination must not hold health or explanation reads.
func TestCronRecorderReadsDuringSlowLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	pipe, err := os.OpenFile(path, os.O_RDWR, 0o600) //nolint:gosec // G304: test-controlled FIFO under t.TempDir()
	require.NoError(t, err)
	recorder := daemon.NewCronDispatchRecorder(path)
	detail := strings.Repeat("x", 1<<20)
	done := make(chan error, 1)
	go func() {
		_, err := recorder.Record(t.Context(), db.RecurringCronCandidate{ProjectID: 1, JobUID: "job"}, db.DispatchRecurringCronOut{Reason: cron.DispatchError}, errors.New(detail), time.Now())
		done <- err
	}()
	// Reading one byte proves the append began; the remaining megabyte stalls it.
	_, err = pipe.Read(make([]byte, 1))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := io.CopyN(io.Discard, pipe, int64(len(detail)))
		require.NoError(t, err)
		require.NoError(t, <-done)
		require.NoError(t, recorder.Close())
		require.NoError(t, pipe.Close())
	})
	read := make(chan struct{})
	go func() { recorder.Last(1, "job"); recorder.Health(); close(read) }()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("health and history blocked behind diagnostic I/O")
	}
}
