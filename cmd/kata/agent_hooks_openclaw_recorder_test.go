package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// Compile a small native recorder so these tests exercise execFile directly on
// Unix and Windows without shebangs, shells or an additional child Node lookup.
func openClawInstallRecorder(t *testing.T, target string) string {
	t.Helper()
	if runtime.GOOS == "windows" && filepath.Ext(target) != ".exe" {
		target += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", target, "./testdata/openclaw/recorder") //nolint:gosec // G204: fixed Go recorder source and isolated output path.
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	return target
}
func openClawCopyRecorder(t *testing.T, source, target string) {
	t.Helper()
	if err := os.Link(source, target); err == nil {
		return
	}
	from, err := os.Open(source) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	require.NoError(t, err)
	defer func() { require.NoError(t, from.Close()) }()
	to, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0700) //nolint:gosec // G302: copied isolated recorder must be executable.
	require.NoError(t, err)
	_, err = io.Copy(to, from)
	require.NoError(t, err)
	require.NoError(t, to.Close())
}
