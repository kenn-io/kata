package jsonlog

import (
	"bufio"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A fresh daemon home must accept records, rotate them, and recover after a
// rotation leaves no active handle. Hook logging retains its separate existing contract.
func TestAppenderCreatesRotatesAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "storage", "dispatch.jsonl")
	a, err := Open(t.Context(), path, 1, 2)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	require.NoError(t, a.Append(t.Context(), map[string]int{"n": 1}))
	require.NoError(t, a.Append(t.Context(), map[string]int{"n": 2}))
	require.NoError(t, a.Append(t.Context(), map[string]int{"n": 3}))
	raw, err := os.ReadFile(path + ".2") //nolint:gosec // G304: test-controlled path under t.TempDir()
	require.NoError(t, err)
	require.JSONEq(t, `{"n":2}`, string(raw))
	_, err = os.Stat(path + ".3")
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, a.file.Close())
	a.file = nil
	require.NoError(t, a.Append(t.Context(), map[string]int{"n": 4}))
	raw, err = os.ReadFile(path + ".1") //nolint:gosec // G304: test-controlled path under t.TempDir()
	require.NoError(t, err)
	require.JSONEq(t, `{"n":4}`, string(raw))
}

// A crash may leave a partial record; a later successful append must stand alone.
func TestAppenderRestartPreservesRecordBoundary(t *testing.T) {
	for _, tail := range []string{`{"n":`, `{"n":1}`} {
		t.Run(tail, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dispatch.jsonl")
			require.NoError(t, os.WriteFile(path, []byte("{\"n\":0}\n"+tail), 0600))
			a, err := Open(t.Context(), path, 1<<20, 2)
			require.NoError(t, err)
			require.NoError(t, a.Append(t.Context(), map[string]int{"n": 2}))
			require.NoError(t, a.Close())
			file, err := os.Open(path)
			require.NoError(t, err)
			defer file.Close()
			scan := bufio.NewScanner(file)
			var values []int
			for scan.Scan() {
				var value map[string]int
				if json.Unmarshal(scan.Bytes(), &value) == nil {
					values = append(values, value["n"])
				}
			}
			require.NoError(t, scan.Err())
			require.Contains(t, values, 0, "complete pre-crash records survive")
			require.Contains(t, values, 2, "successful post-crash append is independently readable")
		})
	}
}
