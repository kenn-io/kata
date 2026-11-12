package signingpolicy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

func TestValidatePolicyRejectsSharedSecrets(t *testing.T) {
	t.Setenv("TEST_SHARED_SIGNING_KEY", strings.Repeat("k", 64))
	keys := []Key{
		{Source: Source{KeyID: "key-a", KeyEnv: "TEST_SHARED_SIGNING_KEY"}, EnrollmentID: 1},
		{Source: Source{KeyID: "key-b", KeyEnv: "TEST_SHARED_SIGNING_KEY"}, EnrollmentID: 2},
	}
	require.ErrorIs(t, ValidatePolicy("https://hub.example", keys), ErrSharedSecret)
}

func TestCheckDistinctSecrets(t *testing.T) {
	secret := strings.Repeat("k", 64)
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		name    string
		files   bool
		expired bool
	}{
		{name: "environment equal"},
		{name: "equal files at different paths", files: true},
		{name: "expired duplicate", expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_SHARED_SIGNING_KEY", secret)
			keys := []Key{
				{Source: Source{KeyID: "key-a", KeyEnv: "TEST_SHARED_SIGNING_KEY"}},
				{Source: Source{KeyID: "key-b", KeyEnv: "TEST_SHARED_SIGNING_KEY"}},
			}
			var paths []string
			if tc.files {
				for i := range keys {
					keyFile := filepath.Join(t.TempDir(), "signing.key")
					require.NoError(t, os.WriteFile(keyFile, []byte(secret), 0600))
					keys[i].KeyEnv = ""
					keys[i].KeyFile = keyFile
					paths = append(paths, keyFile)
				}
			}
			if tc.expired {
				keys[1].NotAfter = now.Unix()
			}
			err := CheckDistinctSecrets(keys, now)
			if tc.expired {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrSharedSecret)
			require.ErrorContains(t, err, "key-a")
			require.ErrorContains(t, err, "key-b")
			require.NotContains(t, err.Error(), secret)
			for _, keyFile := range paths {
				require.NotContains(t, err.Error(), keyFile)
			}
		})
	}
}
