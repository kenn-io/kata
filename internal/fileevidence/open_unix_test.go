//go:build unix

package fileevidence

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// The opened node may differ from preflight. Opening a FIFO must not wait for a writer.
func TestOpenLocalFileCannotBlockOnFIFO(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(root, "node"), 0600))
	dir, err := os.OpenRoot(root)
	require.NoError(t, err)
	defer dir.Close()
	f, err := openLocalFile(dir, "node")
	require.NoError(t, err)
	defer f.Close()
	stat, err := f.Stat()
	require.NoError(t, err)
	require.False(t, stat.Mode().IsRegular())
}
