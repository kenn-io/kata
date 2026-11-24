//go:build unix

package signingpolicy

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestSourceRejectsFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "signing.key")
	require.NoError(t, unix.Mkfifo(fifo, 0600))

	done := make(chan error, 1)
	go func() {
		_, err := (Source{KeyID: "k", KeyFile: fifo}).Load()
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrKey)
	case <-time.After(3 * time.Second):
		t.Fatal("Load blocked on FIFO")
	}
}
