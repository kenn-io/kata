package db_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMutationHelpersDoNotReturnResultsWithCommitCall(t *testing.T) {
	root := filepath.Join("..", "..", "internal", "db")
	riskyReturn := regexp.MustCompile(`return\s+.*,\s*tx\.Commit\(\)`)
	var offenders []string
	require.NoError(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		require.NoError(t, err)
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		bs, err := os.ReadFile(path) //nolint:gosec // test reads checked-in source files.
		require.NoError(t, err)
		lines := strings.Split(string(bs), "\n")
		for i, line := range lines {
			if riskyReturn.MatchString(line) {
				offenders = append(offenders, filepath.ToSlash(path)+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	}))
	require.Empty(t, offenders, "commit errors must be checked before returning mutation result values")
}
