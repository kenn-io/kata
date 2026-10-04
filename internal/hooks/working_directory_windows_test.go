//go:build windows

package hooks

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
	"golang.org/x/sys/windows"
)

func TestDoctorHookDiagnosticsRequireWorkingDirectoryAccessOnWindows(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "working")
	require.NoError(t, safefileio.EnsurePrivateDir(workdir))
	probe := filepath.Join(workdir, "access-probe")
	require.NoError(t, os.WriteFile(probe, []byte("probe"), 0600))
	denyWindowsWorkingDirectoryAccess(t, workdir)
	t.Cleanup(func() { require.NoError(t, safefileio.EnsurePrivateDir(workdir)) })

	withoutWindowsTraversePrivilege(t, func() {
		info, err := os.Stat(workdir)
		require.NoError(t, err, "the test directory remains stat-able")
		require.True(t, info.IsDir())
		path, err := windows.UTF16PtrFromString(probe)
		require.NoError(t, err)
		handle, err := windows.CreateFile(
			path,
			windows.FILE_READ_ATTRIBUTES,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err == nil {
			_ = windows.CloseHandle(handle)
			require.Fail(t, "the directory ACL must deny access to a known child path")
		}
		require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED, "the parent directory ACL must prevent reaching the child")

		d, _, _ := mustNewDispatcher(t, []ResolvedHook{{Command: "unused", WorkingDir: workdir}}, defaultConfig())
		got := d.Diagnostics()
		require.False(t, got.Hooks[0].WorkingDirectoryAvailable)
	})
}

func withoutWindowsTraversePrivilege(t *testing.T, fn func()) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var processToken windows.Token
	require.NoError(t, windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY,
		&processToken,
	))
	defer func() { require.NoError(t, processToken.Close()) }()

	var threadToken windows.Token
	require.NoError(t, windows.DuplicateTokenEx(
		processToken,
		windows.TOKEN_QUERY|windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_IMPERSONATE,
		nil,
		windows.SecurityImpersonation,
		windows.TokenImpersonation,
		&threadToken,
	))
	defer func() { require.NoError(t, threadToken.Close()) }()

	privilegeName, err := windows.UTF16PtrFromString("SeChangeNotifyPrivilege")
	require.NoError(t, err)
	var traversePrivilege windows.LUID
	require.NoError(t, windows.LookupPrivilegeValue(nil, privilegeName, &traversePrivilege))
	privileges := windows.Tokenprivileges{PrivilegeCount: 1}
	privileges.Privileges[0] = windows.LUIDAndAttributes{Luid: traversePrivilege}
	previous := windows.Tokenprivileges{}
	var previousSize uint32
	require.NoError(t, windows.AdjustTokenPrivileges(
		threadToken,
		false,
		&privileges,
		uint32(unsafe.Sizeof(previous)),
		&previous,
		&previousSize,
	))
	require.Equal(t, uint32(1), previous.PrivilegeCount, "the test token must include SeChangeNotifyPrivilege")
	require.Equal(t, traversePrivilege, previous.Privileges[0].Luid)
	require.NotZero(t, previous.Privileges[0].Attributes&windows.SE_PRIVILEGE_ENABLED, "the test must disable an enabled traverse privilege")
	require.NoError(t, windows.SetThreadToken(nil, threadToken))
	defer func() { require.NoError(t, windows.RevertToSelf()) }()

	fn()
}

func denyWindowsWorkingDirectoryAccess(t *testing.T, dir string) {
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
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	trustee := windows.TRUSTEE{
		TrusteeForm:  windows.TRUSTEE_IS_SID,
		TrusteeType:  windows.TRUSTEE_IS_USER,
		TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{AccessPermissions: windows.FILE_TRAVERSE, AccessMode: windows.DENY_ACCESS, Trustee: trustee},
		{AccessPermissions: windows.FILE_READ_ATTRIBUTES, AccessMode: windows.GRANT_ACCESS, Trustee: trustee},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	))
}
