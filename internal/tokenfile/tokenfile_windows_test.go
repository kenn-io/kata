//go:build windows

package tokenfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestReserveWindowsRequiresPrivateDirectoryAndExclusiveDestination(t *testing.T) {
	root := t.TempDir()
	user, _, err := privateDirectoryTrustees()
	require.NoError(t, err)

	privateDir := filepath.Join(root, "private")
	require.NoError(t, os.Mkdir(privateDir, 0o700))
	setDirectorySDDL(t, privateDir,
		"D:P(A;OICI;FA;;;"+user.String()+")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	path := filepath.Join(privateDir, "worker.token")
	reservation, err := Reserve(path)
	require.NoError(t, err)
	require.NoError(t, reservation.Commit("secret"))
	_, err = Reserve(path)
	require.ErrorContains(t, err, "already exists")

	broadDir := filepath.Join(root, "broad")
	require.NoError(t, os.Mkdir(broadDir, 0o700))
	setDirectorySDDL(t, broadDir,
		"D:P(A;OICI;FA;;;"+user.String()+")(A;OICI;GR;;;WD)")
	_, err = Reserve(filepath.Join(broadDir, "worker.token"))
	require.ErrorContains(t, err, "must be owner-only")
}

func setDirectorySDDL(t *testing.T, path, sddl string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	require.NoError(t, err)
	dacl, _, err := descriptor.DACL()
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	))
}
