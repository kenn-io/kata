package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

var externalFixtures struct {
	sync.Mutex
	dir      string
	binaries map[string]string
}

// Compile external contract fixtures before their behavioral deadlines start.
// They exercise real subprocess I/O and cleanup without CLI test initialization.
func externalFixtureBinary(t *testing.T, name string) string {
	t.Helper()
	externalFixtures.Lock()
	defer externalFixtures.Unlock()
	if bin := externalFixtures.binaries[name]; bin != "" {
		return bin
	}
	if externalFixtures.dir == "" {
		dir, err := os.MkdirTemp("", "kata-contract-fixtures-")
		require.NoError(t, err)
		externalFixtures.dir = dir
		externalFixtures.binaries = map[string]string{}
	}
	filename := name
	if runtime.GOOS == "windows" {
		filename += ".exe"
	}
	bin := filepath.Join(externalFixtures.dir, filename)
	//nolint:gosec // Fixed testdata package names and owned binary destination.
	output, err := exec.Command("go", "build", "-o", bin, "./testdata/"+name).CombinedOutput()
	require.NoError(t, err, "build external contract fixture: %s", output)
	externalFixtures.binaries[name] = bin
	return bin
}
func cleanupExternalFixtures() error {
	if externalFixtures.dir == "" {
		return nil
	}
	return os.RemoveAll(externalFixtures.dir)
}
