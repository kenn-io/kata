//go:build windows

package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kit/safefileio"
	"golang.org/x/sys/windows"
)

func TestReadAuthConfigRejectsCredentialReadableByOtherWindowsUsers(t *testing.T) {
	home := t.TempDir()
	tokenFile := filepath.Join(home, "mounted-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("example-token\n"), 0o600))
	grantWindowsEveryoneAccess(t, tokenFile)
	file, err := os.Open(tokenFile) //nolint:gosec // tokenFile is created inside t.TempDir.
	require.NoError(t, err)
	require.Error(t, safefileio.ValidatePrivateCurrentUserFile(file), "fixture must grant access to Everyone")
	require.NoError(t, file.Close())
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte(fmt.Sprintf("[auth]\ntoken_file = %q\n", tokenFile)), 0o600))
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	previousTokenFile, hadTokenFile := os.LookupEnv("KATA_AUTH_TOKEN_FILE")
	require.NoError(t, os.Unsetenv("KATA_AUTH_TOKEN_FILE"))
	t.Cleanup(func() {
		if hadTokenFile {
			_ = os.Setenv("KATA_AUTH_TOKEN_FILE", previousTokenFile)
		} else {
			_ = os.Unsetenv("KATA_AUTH_TOKEN_FILE")
		}
	})

	_, err = config.ReadAuthConfig()
	require.Error(t, err)
	require.Contains(t, err.Error(), "private")
	require.NotContains(t, err.Error(), "example-token")
}

func grantWindowsEveryoneAccess(t *testing.T, path string) {
	t.Helper()
	path16, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	handle, err := windows.CreateFile(
		path16,
		windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	require.NoError(t, err)
	defer func() { _ = windows.CloseHandle(handle) }()
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, err)
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		windowsCredentialAccess(current.User.Sid, windows.TRUSTEE_IS_USER, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
		windowsCredentialAccess(everyone, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
	}, nil)
	require.NoError(t, err)
	require.NoError(t, windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil))
}

func windowsCredentialAccess(sid *windows.SID, trusteeType windows.TRUSTEE_TYPE, inheritance uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
