package mcpevents_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/mcpevents"
)

func TestKeyFileProviderMissing(t *testing.T) {
	p := mcpevents.NewFileKeyProvider(filepath.Join(t.TempDir(), "rt", "mcp-events.key"))
	_, err := p.Load(context.Background())
	assert.ErrorIs(t, err, mcpevents.ErrKeyMissing)
}

func TestKeyFileProviderConcurrentCreateConverges(t *testing.T) {
	dir := t.TempDir()
	path := config.MCPEventsKeyPath(dir, "storage1")
	const n = 8
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			keys[i], errs[i] = mcpevents.NewFileKeyProvider(path).Create(context.Background())
		})
	}
	close(start)
	wg.Wait()
	for i := range n {
		require.NoError(t, errs[i])
		assert.Len(t, keys[i], 32)
		assert.Equal(t, keys[0], keys[i])
	}
	loaded, err := mcpevents.NewFileKeyProvider(path).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, keys[0], loaded)

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp files may remain")
	assert.Equal(t, "mcp-events.key", entries[0].Name())
	raw, err := os.ReadFile(path) //nolint:gosec // Test-owned temporary key path.
	require.NoError(t, err)
	assert.Len(t, raw, 32)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestKeyFileProviderCreateKeepsExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-events.key")
	p := mcpevents.NewFileKeyProvider(path)
	first, err := p.Create(context.Background())
	require.NoError(t, err)
	second, err := p.Create(context.Background())
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestKeyFileProviderRejectsCorruptKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-events.key")
	require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))
	p := mcpevents.NewFileKeyProvider(path)
	_, err := p.Load(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, mcpevents.ErrKeyMissing)
	_, err = p.Create(context.Background())
	require.Error(t, err, "a corrupt key is never replaced")
	raw, err := os.ReadFile(path) //nolint:gosec // Test-owned temporary key path.
	require.NoError(t, err)
	assert.Equal(t, []byte("short"), raw)
}

func TestKeyFileProviderRejectsOpenPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	path := filepath.Join(t.TempDir(), "mcp-events.key")
	require.NoError(t, os.WriteFile(path, make([]byte, 32), 0o644)) //nolint:gosec // Deliberately insecure fixture permissions exercise rejection.
	require.NoError(t, os.Chmod(path, 0o644))                       //nolint:gosec // Deliberately insecure fixture permissions exercise rejection.
	_, err := mcpevents.NewFileKeyProvider(path).Load(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, mcpevents.ErrKeyMissing)
}

func TestKeyCheckIsHexHMACOfConstant(t *testing.T) {
	key := make([]byte, 32)
	got := mcpevents.KeyCheck(key)
	assert.Len(t, got, 64)
	assert.Equal(t, got, mcpevents.KeyCheck(key))
	other := make([]byte, 32)
	other[0] = 1
	assert.NotEqual(t, got, mcpevents.KeyCheck(other))
	// HMAC-SHA256(zero key, "kata-mcp-events-key-check"), pinned.
	assert.Equal(t, "5d8dbf37f9d7e2d73d0e6949b83d091ecc795905f2895dc104e8d0b623bf973e", got)
}
