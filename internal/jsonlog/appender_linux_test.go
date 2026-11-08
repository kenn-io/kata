package jsonlog

import (
	"encoding/json/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// A successful append after a partial write must remain independently readable.
// Limit file size in a subprocess so other tests keep their process limits.
func TestAppenderPartialWritePreservesNextRecord(t *testing.T) {
	const child = "JSONLOG_PARTIAL_WRITE_CHILD"
	if os.Getenv(child) != "1" {
		binary, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestAppenderPartialWritePreservesNextRecord$", "-test.v") //nolint:gosec // G204: current test executable, fixed arguments
		cmd.Env = append(os.Environ(), child+"=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}

	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	a, err := Open(t.Context(), path, 1<<20, 2)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	var original syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original))
	signal.Ignore(syscall.SIGXFSZ)
	limited := original
	limited.Cur = 16
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited))
	t.Cleanup(func() { require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original)) })
	writeErr := a.Append(t.Context(), map[string]string{"payload": strings.Repeat("x", 128)})
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original))
	require.ErrorIs(t, writeErr, syscall.EFBIG)
	partial, err := os.ReadFile(path) //nolint:gosec // G304: TempDir fixture
	require.NoError(t, err)
	require.Len(t, partial, 16, "the failed append actually wrote a fragment")
	require.NotContains(t, string(partial), "\n")

	require.NoError(t, a.Append(t.Context(), map[string]int{"n": 2}))
	require.NoError(t, a.Close())
	raw, err := os.ReadFile(path) //nolint:gosec // G304: TempDir fixture
	require.NoError(t, err)
	var values []int
	for line := range strings.SplitSeq(string(raw), "\n") {
		var value map[string]int
		if json.Unmarshal([]byte(line), &value) == nil {
			values = append(values, value["n"])
		}
	}
	require.Contains(t, values, 2, "successful append after a failed partial write is independently readable")
}
