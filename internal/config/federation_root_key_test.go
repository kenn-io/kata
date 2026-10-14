package config_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestRootSigningKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "root.key")
	_, err := config.ReadRootSigningKey(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "reading never creates a key")
	first, err := config.LoadOrCreateRootSigningKey(path)
	require.NoError(t, err)
	require.Len(t, first, ed25519.PrivateKeySize)
	same, err := config.ReadRootSigningKey(path)
	require.NoError(t, err)
	require.Equal(t, first, same)
	again, err := config.LoadOrCreateRootSigningKey(path)
	require.NoError(t, err)
	require.Equal(t, first, again, "an existing key is never replaced")
	_, err = config.ReadRootSigningKey("root.key")
	require.Error(t, err, "relative paths are refused")
}
func TestRootSigningKeyConcurrentCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root.key")
	var wg sync.WaitGroup
	keys := make([]ed25519.PrivateKey, 2)
	errors := make([]error, 2)
	for i := range keys {
		wg.Go(func() { keys[i], errors[i] = config.LoadOrCreateRootSigningKey(path) })
	}
	wg.Wait()
	for _, err := range errors {
		require.NoError(t, err)
	}
	require.Equal(t, keys[0], keys[1], "one instance must never start with competing fresh signing identities")
}
func TestRootSigningKeyRejectsUnsafeOrMalformedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		mode        os.FileMode
	}{{"bad encoding", "not-a-key", 0600}, {"bad size", "YWJj", 0600}, {"world-readable", base64.StdEncoding.EncodeToString(make([]byte, ed25519.SeedSize)), 0644}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "world-readable" && runtime.GOOS == "windows" {
				t.Skip("Unix permission boundary")
			}
			path := filepath.Join(t.TempDir(), "root.key")
			require.NoError(t, os.WriteFile(path, []byte(tc.value), tc.mode))
			_, err := config.LoadOrCreateRootSigningKey(path)
			require.Error(t, err)
		})
	}
	_, err := config.LoadOrCreateRootSigningKey(t.TempDir())
	require.Error(t, err, "directories are not key files")
}

// Surrounding whitespace decodes cleanly, so only the 256-byte file limit can
// reject the padded copy of an otherwise valid key.
func TestRootSigningKeyFileSizeLimit(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	encoded := base64.StdEncoding.EncodeToString(seed)
	atLimit := encoded + strings.Repeat(" ", 256-len(encoded))
	path := filepath.Join(t.TempDir(), "root.key")
	require.NoError(t, os.WriteFile(path, []byte(atLimit), 0o600))
	key, err := config.LoadOrCreateRootSigningKey(path)
	require.NoError(t, err)
	require.Equal(t, ed25519.NewKeyFromSeed(seed), key)

	overLimit := atLimit + " "
	path = filepath.Join(t.TempDir(), "root.key")
	require.NoError(t, os.WriteFile(path, []byte(overLimit), 0o600))
	_, err = config.LoadOrCreateRootSigningKey(path)
	require.EqualError(t, err, "root signing key must be a small owner-only regular file")
}
