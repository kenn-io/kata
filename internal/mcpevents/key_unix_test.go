//go:build unix

package mcpevents_test

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/mcpevents"
)

// A FIFO at the key path must fail the load promptly. A blocking open would
// hang daemon start until some process opened the other end.
func TestKeyFileProviderRejectsAFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-events.key")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	provider := mcpevents.NewFileKeyProvider(path)

	done := make(chan error, 1)
	go func() {
		_, err := provider.Load(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.NotErrorIs(t, err, mcpevents.ErrKeyMissing, "a FIFO is a bad key, not a missing one")
	case <-time.After(10 * time.Second):
		require.FailNow(t, "loading the key blocked on a FIFO")
	}
}
