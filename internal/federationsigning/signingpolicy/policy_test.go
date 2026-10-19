package signingpolicy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceLoadsOwnerOnlyKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	secret := strings.Repeat("k", 64)
	require.NoError(t, os.WriteFile(path, []byte(secret), 0600))

	got, err := (Source{KeyID: "key-a", KeyFile: path}).Load()
	require.NoError(t, err)
	require.Equal(t, []byte(secret), got)
}

func TestSourceRejectsNonRegularKeyFile(t *testing.T) {
	dir := t.TempDir()

	got, err := (Source{KeyID: "key-a", KeyFile: dir}).Load()
	require.ErrorIs(t, err, ErrKey)
	require.Nil(t, got)
}

func TestSourceRejectsKeyFileReadableByOthersOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permission contract")
	}
	path := filepath.Join(t.TempDir(), "signing.key")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("k", 64)), 0600))
	require.NoError(t, os.Chmod(path, 0644)) // #nosec G302 -- intentionally makes the test key readable to prove rejection.

	got, err := (Source{KeyID: "key-a", KeyFile: path}).Load()
	require.ErrorIs(t, err, ErrKey)
	require.Nil(t, got)
}
