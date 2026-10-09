package config_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestRootSigningKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "root.key")
	first, err := config.LoadOrCreateRootSigningKey(path, "")
	require.NoError(t, err)
	require.Len(t, first, ed25519.PrivateKeySize)
	digest := sha256.Sum256(first.Public().(ed25519.PublicKey))
	id := hex.EncodeToString(digest[:])
	same, err := config.LoadOrCreateRootSigningKey(path, id)
	require.NoError(t, err)
	require.Equal(t, first, same)
	_, err = config.LoadOrCreateRootSigningKey(path, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.Error(t, err)
	require.NoError(t, os.Remove(path))
	_, err = config.LoadOrCreateRootSigningKey(path, id)
	require.Error(t, err, "missing key at known authority requires explicit recovery/repinning")
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "failure must not silently create replacement")
}
func TestRootSigningKeyConcurrentCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root.key")
	var wg sync.WaitGroup
	keys := make([]ed25519.PrivateKey, 2)
	errors := make([]error, 2)
	for i := range keys {
		wg.Go(func() { keys[i], errors[i] = config.LoadOrCreateRootSigningKey(path, "") })
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
	}{{"bad encoding", "not-a-key", 0600}, {"bad size", "YWJj", 0600}, {"oversize", string(make([]byte, 1024)), 0600}, {"world-readable", base64.StdEncoding.EncodeToString(make([]byte, ed25519.SeedSize)), 0644}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "world-readable" && runtime.GOOS == "windows" {
				t.Skip("Unix permission boundary")
			}
			path := filepath.Join(t.TempDir(), "root.key")
			require.NoError(t, os.WriteFile(path, []byte(tc.value), tc.mode))
			_, err := config.LoadOrCreateRootSigningKey(path, "")
			require.Error(t, err)
		})
	}
	_, err := config.LoadOrCreateRootSigningKey(t.TempDir(), "")
	require.Error(t, err, "directories are not key files")
}
