//go:build windows

package client

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
	"golang.org/x/sys/windows"
)

func makeRuntimeDirectoryInsecureForTest(t *testing.T, dir string) func() {
	t.Helper()
	require.NoError(t, safefileio.EnsurePrivateDir(dir))
	makeTestDirectoryDACLUnprotected(t, dir)
	before := safefileio.ValidatePrivateDir(dir)
	require.Error(t, before)
	return func() {
		t.Helper()
		require.EqualError(t, safefileio.ValidatePrivateDir(dir), before.Error(), "read-only discovery must not repair the DACL")
	}
}

func makeTestDirectoryDACLUnprotected(t *testing.T, dir string) {
	t.Helper()
	path, err := windows.UTF16PtrFromString(dir)
	require.NoError(t, err)
	handle, err := windows.CreateFile(
		path,
		windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	require.NoError(t, err)
	defer func() { _ = windows.CloseHandle(handle) }()
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	dacl, _, err := descriptor.DACL()
	require.NoError(t, err)
	require.NoError(t, windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	))
}
