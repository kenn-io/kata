package jsonlog

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Records must be complete JSONL lines, rotation bounded, and a failed open
// retryable without replacing the appender or interrupting dispatch.
func TestAppenderRotationAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "dispatch.jsonl")
	a := New(path, 8, 2)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	for _, value := range []string{"one", "two", "three"} {
		require.NoError(t, a.Append(t.Context(), value))
	}
	raw, err := os.ReadFile(path + ".1") //nolint:gosec // Test-owned path inside t.TempDir.
	require.NoError(t, err)
	require.Equal(t, "\"three\"\n", string(raw))
	raw, err = os.ReadFile(path + ".2") //nolint:gosec // Test-owned path inside t.TempDir.
	require.NoError(t, err)
	require.Equal(t, "\"one\"\n\"two\"\n", string(raw))
	_, err = os.Stat(path + ".3")
	require.ErrorIs(t, err, os.ErrNotExist)

	blocked := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("file"), 0o600))
	b := New(filepath.Join(blocked, "dispatch.jsonl"), 100, 1)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.Error(t, b.Append(t.Context(), "lost"))
	require.NoError(t, os.Remove(blocked))
	require.NoError(t, b.Append(t.Context(), "recovered"))
	raw, err = os.ReadFile(filepath.Join(blocked, "dispatch.jsonl")) //nolint:gosec // Test-owned path inside t.TempDir.
	require.NoError(t, err)
	require.Equal(t, "\"recovered\"\n", string(raw))
}

func TestAppenderConcurrentLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	a := New(path, 10000, 1)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() { require.NoError(t, a.Append(t.Context(), "line")) })
	}
	wg.Wait()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 30*len("\"line\"\n"), len(raw))
}

// A restart or short write can leave a fragment; the next successful append
// must restore a record boundary instead of joining that fragment.
func TestAppenderRecoversUnterminatedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":`), 0o600))
	a := New(path, 0, 0)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	require.NoError(t, a.Append(t.Context(), map[string]int{"version": 1}))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "{\"version\":\n{\"version\":1}\n", string(raw))
}
