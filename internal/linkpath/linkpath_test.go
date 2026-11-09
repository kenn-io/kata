package linkpath_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/linkpath"
)

// A file directly under a filesystem root keeps its absolute path; an empty
// parent must mean the root, not the working directory.
func TestChainKeepsFileUnderFilesystemRoot(t *testing.T) {
	root := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	path := root + "kata-linkpath-missing.db"

	require.Equal(t, []string{path}, linkpath.Chain(path))
}
