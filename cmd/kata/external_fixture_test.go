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
	return testBinary(t, name, "./testdata/"+name)
}

func kataTestBinary(t *testing.T, version string) string {
	t.Helper()
	if version == "" {
		return testBinary(t, "kata-dev", "-buildvcs=false", "go.kenn.io/kata/cmd/kata")
	}
	return testBinary(t, "kata-"+version, "-buildvcs=false", "-ldflags", "-X go.kenn.io/kata/internal/version.Version="+version, "go.kenn.io/kata/cmd/kata")
}

func testBinary(t *testing.T, name string, args ...string) string {
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
	//nolint:gosec // Fixed test build arguments and owned binary destination.
	output, err := exec.CommandContext(t.Context(), "go", append([]string{"build", "-o", bin}, args...)...).CombinedOutput()
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
