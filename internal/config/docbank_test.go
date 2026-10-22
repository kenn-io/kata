package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func readDocbank(t *testing.T, body string) (*config.DaemonConfig, error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600))
	return config.ReadDaemonConfig()
}
func TestDocbankConfig(t *testing.T) {
	cfg, err := readDocbank(t, `[docbank]
url="https://docbank.example"
api_key="test-key"
`)
	require.NoError(t, err)
	require.True(t, cfg.Docbank.Enabled())
	require.Equal(t, "/kata", cfg.Docbank.Folder)
	for _, u := range []string{"http://localhost:9000", "http://127.0.0.1:9000", "http://[::1]:9000", "https://docbank.example/prefix"} {
		_, err = readDocbank(t, fmt.Sprintf("[docbank]\nurl=%q\napi_key=\"test-key\"\n", u))
		require.NoError(t, err)
	}
	for _, u := range []string{"http://docbank.example"} {
		_, err = readDocbank(t, fmt.Sprintf("[docbank]\nurl=%q\napi_key=\"test-key\"\n", u))
		require.Error(t, err, u)
	}
	for _, extra := range []string{"api_key=\"\"", "api_key=\"test-key\"\nfolder=\"relative\"", "api_key=\"test-key\"\nfolder=\"/a/../b\""} {
		_, err = readDocbank(t, "[docbank]\nurl=\"https://docbank.example\"\n"+extra)
		require.Error(t, err, extra)
	}
}
func TestDocbankConfigDefersKeyResolution(t *testing.T) {
	t.Setenv("DOCBANK_MISSING_TEST_KEY", "")
	missingFile := filepath.Join(t.TempDir(), "missing-key")
	for name, source := range map[string]string{
		"environment": `api_key_env="DOCBANK_MISSING_TEST_KEY"`,
		"file":        fmt.Sprintf("api_key_file=%q", missingFile),
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := readDocbank(t, "[docbank]\nurl=\"https://docbank.example\"\n"+source)
			require.NoError(t, err, "unavailable request credentials must not break unrelated config reads")
			_, err = cfg.Docbank.ResolveKey()
			require.Error(t, err, "the configured secret source is resolved when Docbank is used")
		})
	}

	file := filepath.Join(t.TempDir(), "key")
	writePrivateCredentialFixture(t, file, []byte("first"))
	cfg, err := readDocbank(t, fmt.Sprintf("[docbank]\nurl=\"https://docbank.example\"\napi_key_file=%q", file))
	require.NoError(t, err)
	key, err := cfg.Docbank.ResolveKey()
	require.NoError(t, err)
	require.Equal(t, "first", key)
	require.NoError(t, os.WriteFile(file, []byte("rotated"), 0600))
	key, err = cfg.Docbank.ResolveKey()
	require.NoError(t, err)
	require.Equal(t, "rotated", key)
}
