//go:build !windows

package signingpolicy

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSourceRejectsFIFOKeyFile(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "signing.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0600))

	errs := make(chan error, 1)
	go func() {
		_, err := (Source{KeyID: "k", KeyFile: fifo}).Load()
		errs <- err
	}()

	select {
	case err := <-errs:
		require.ErrorIs(t, err, ErrKey)
	case <-time.After(3 * time.Second):
		t.Fatal("Load blocked on FIFO")
	}
}
