package main

import (
	"io"
	"os"
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
	openClawCopyRecorder(t, testBinary(t, "openclaw-recorder", "./testdata/openclaw/recorder"), target)
	return target
}

func TestOpenClawCopyRecorderKeepsLinkedSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(source, []byte("recorder"), 0700))
	openClawCopyRecorder(t, source, target)
	openClawCopyRecorder(t, source, target)
	for _, path := range []string{source, target} {
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "recorder", string(content))
	}
}

func openClawCopyRecorder(t *testing.T, source, target string) {
	t.Helper()
	if err := os.Remove(target); !os.IsNotExist(err) {
		require.NoError(t, err)
	}
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
