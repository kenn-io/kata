package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kit/safefileio"
)

// Contract: inline > file > named env, with no fallback from a bad source.
func TestAuthCredentialSources(t *testing.T) {
	for _, tc := range []struct {
		name, inline, fileContents, env, legacy, want string
		missing, wantError                            bool
	}{
		{name: "file before env", fileContents: " file-secret\n", env: "env-secret", want: "file-secret"},
		{name: "inline before missing file", inline: "inline-secret", missing: true, env: "env-secret", want: "inline-secret"},
		{name: "legacy env override", inline: "inline-secret", missing: true, legacy: "legacy-secret", want: "legacy-secret"},
		{name: "missing file never falls back", missing: true, env: "env-secret", wantError: true},
		{name: "empty file never falls back", fileContents: " \n", env: "env-secret", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			t.Setenv("KATA_AUTH_TOKEN", tc.legacy)
			t.Setenv("EXAMPLE_AUTH_TOKEN", tc.env)
			file := filepath.Join(home, "mounted-token")
			if !tc.missing {
				writePrivateCredentialFixture(t, file, []byte(tc.fileContents))
			}
			body := fmt.Sprintf("[auth]\ntoken = %q\ntoken_file = %q\ntoken_env = \"EXAMPLE_AUTH_TOKEN\"\n", tc.inline, file)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))
			for name, read := range map[string]func() (string, error){
				"daemon": func() (string, error) {
					cfg, err := config.ReadDaemonConfig()
					if err != nil {
						return "", err
					}
					return cfg.Auth.Token, nil
				},
				"auth client": func() (string, error) { cfg, err := config.ReadAuthConfig(); return cfg.Token, err },
			} {
				t.Run(name, func(t *testing.T) {
					got, err := read()
					if tc.wantError {
						require.Error(t, err)
						require.NotContains(t, err.Error(), "env-secret")
						return
					}
					require.NoError(t, err)
					require.Equal(t, tc.want, got)
				})
			}
		})
	}
}

func TestAuthCredentialEnvironmentFileAndInvalidNamedEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	file := filepath.Join(home, "token")
	writePrivateCredentialFixture(t, file, []byte("file-secret\n"))
	t.Setenv("KATA_AUTH_TOKEN_FILE", file)
	cfg, err := config.ReadAuthConfig()
	require.NoError(t, err)
	require.Equal(t, "file-secret", cfg.Token)
	t.Setenv("KATA_AUTH_TOKEN_FILE", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[auth]\ntoken_env = \"EXAMPLE_EMPTY_TOKEN\"\n"), 0o600))
	for _, value := range []string{"", " \n"} {
		t.Setenv("EXAMPLE_EMPTY_TOKEN", value)
		_, err := config.ReadAuthConfig()
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "credential"))
	}
}

func TestAuthPolicyReadersPreservePolicyAndCatalogWithoutLocalCredential(t *testing.T) {
	home := t.TempDir()
	localTokenFile := filepath.Join(home, "missing-local-token")
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "environment-local-token")
	t.Setenv("KATA_AUTH_TOKEN_FILE", localTokenFile)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_BACKUP_DIR", "")
	t.Setenv("KATA_BACKUP_INTERVAL", "1h")
	t.Setenv("KATA_BACKUP_RETAIN", "")
	configBody := fmt.Sprintf(`[auth]
trust_private_network = true
token_file = %q

[[daemon]]
name = "hub"
url = "https://hub.example"
token = "catalog-hub-token"
`, localTokenFile)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600))

	cfg, err := config.ReadDaemonCatalogAndAuthPolicy()
	require.NoError(t, err)
	require.Len(t, cfg.Daemons, 1)
	require.Equal(t, "catalog-hub-token", cfg.Daemons[0].Token)
	require.True(t, cfg.Auth.TrustPrivateNetwork)
	require.Empty(t, cfg.Auth.Token)
	require.Empty(t, cfg.Auth.TokenFile)
	require.Empty(t, cfg.Auth.TokenEnv)

	auth, err := config.ReadAuthConfigForPolicy()
	require.NoError(t, err)
	require.True(t, auth.TrustPrivateNetwork)
	require.Empty(t, auth.Token)
	require.Empty(t, auth.TokenFile)
	require.Empty(t, auth.TokenEnv)
}

func TestLocalProfileForwardsAuthCredentialReference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EXAMPLE_PROFILE_TOKEN", "profile-secret")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[auth]\ntoken_env = \"EXAMPLE_PROFILE_TOKEN\"\n"), 0o600))
	cfg, err := config.ReadDaemonConfigForHome(home)
	require.NoError(t, err)
	env, err := config.LocalProfileEnvironment(config.LocalProfileConfig{Home: home, Config: cfg}, true)
	require.NoError(t, err)
	require.Contains(t, env, "EXAMPLE_PROFILE_TOKEN=profile-secret")
}

// A relative KATA_HOME must keep its persisted owner credential usable.
func TestReadPersistedAuthTokenRelativeHome(t *testing.T) {
	t.Chdir(t.TempDir())
	home := filepath.Join(".", "data")
	require.NoError(t, os.Mkdir(home, 0o700))
	writePrivateCredentialFixture(t, filepath.Join(home, "auth-token"), []byte("example-secret\n"))

	token, err := config.ReadPersistedAuthToken(home)
	require.NoError(t, err)
	require.Equal(t, "example-secret", token)
}

func writePrivateCredentialFixture(t *testing.T, path string, contents []byte) {
	t.Helper()
	file, err := safefileio.CreatePrivateFile(path)
	require.NoError(t, err)
	_, err = file.Write(contents)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}
