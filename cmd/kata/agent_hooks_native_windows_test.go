//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// Harness runtimes such as Node open files with FILE_SHARE_DELETE. A reader
// holding the native config that way must not make the update fail.
func TestNativeAgentHookPublishReplacesFileHeldByDeleteSharingReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o600))
	name, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = windows.CloseHandle(h) })

	plan := nativeAgentHookPlan{Changes: []nativeAgentHookChange{{Path: path, Original: []byte("before"), OriginalExists: true, Content: []byte("after")}}}
	changed, err := publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	require.True(t, changed)
	got, err := os.ReadFile(path) //nolint:gosec // G304: test-owned temporary file.
	require.NoError(t, err)
	require.Equal(t, "after", string(got))
}
