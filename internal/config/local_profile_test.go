package config_test

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestDaemonConfigLocalProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	workHome := filepath.Join(t.TempDir(), "work-home")
	body := fmt.Sprintf(`[[daemon]]
name = "work"
local = true
home = %q
instance_uid = "01HZZZZZZZZZZZZZZZZZZZZZ01"
`, workHome)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Len(t, cfg.Daemons, 1)
	assert.Equal(t, "work", cfg.Daemons[0].Name)
	assert.True(t, cfg.Daemons[0].Local)
	assert.Equal(t, workHome, cfg.Daemons[0].Home)
	assert.Equal(t, "01HZZZZZZZZZZZZZZZZZZZZZ01", cfg.Daemons[0].InstanceUID)
}

func TestLocalConfigRejectsURLAndDaemon(t *testing.T) {
	root := t.TempDir()
	writeKataLocal(t, root, `version = 1
[server]
url = "http://127.0.0.1:7780"
daemon = "work"
`)
	_, err := config.ReadLocalConfig(root)
	require.Error(t, err)
}

func TestDaemonConfigLocalProfileRejectsIncompleteIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, target, fields string
	}{
		{"home only", "local = true", "home = %q"},
		{"identity only", "local = true", `instance_uid = "01HZZZZZZZZZZZZZZZZZZZZZ01"`},
		{"remote home", `url = "https://daemon.example"`, "home = %q\ninstance_uid = \"01HZZZZZZZZZZZZZZZZZZZZZ01\""},
		{"relative home", "local = true", `home = "work-home"` + "\n" + `instance_uid = "01HZZZZZZZZZZZZZZZZZZZZZ01"`},
		{"invalid identity", "local = true", "home = %q\ninstance_uid = \"invalid\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			fields := tc.fields
			if strings.Contains(fields, "%q") {
				fields = fmt.Sprintf(fields, filepath.Join(t.TempDir(), "work-home"))
			}
			body := "[[daemon]]\nname = \"work\"\n" + tc.target + "\n" + fields + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))
			_, err := config.ReadDaemonConfig()
			require.Error(t, err)
		})
	}
}

func TestDaemonConfigLocalProfileExpandsUserHome(t *testing.T) {
	home := t.TempDir()
	userHome := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[[daemon]]
name = "work"
local = true
home = " ~/work-home "
instance_uid = " 01HZZZZZZZZZZZZZZZZZZZZZ01 "
`), 0o600))
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(userHome, "work-home"), cfg.Daemons[0].Home)
	assert.Equal(t, "01HZZZZZZZZZZZZZZZZZZZZZ01", cfg.Daemons[0].InstanceUID)
}

func TestReadDaemonConfigForHomeIgnoresParentOverrides(t *testing.T) {
	personalHome := t.TempDir()
	workHome := t.TempDir()
	t.Setenv("KATA_HOME", personalHome)
	t.Setenv("KATA_AUTH_TOKEN", "personal-token")
	t.Setenv("KATA_POSTGRES_SCHEMA", "personal_schema")
	t.Setenv("KATA_AUTOSTART_IDLE_TIMEOUT", "invalid")
	require.NoError(t, os.WriteFile(filepath.Join(workHome, "config.toml"), []byte(`
listen = "127.0.0.1:7780"
[auth]
token = "work-token"
[storage.postgres]
schema = "work_schema"
`), 0o600))

	cfg, err := config.ReadDaemonConfigForHome(workHome)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7780", cfg.Listen)
	assert.Equal(t, "work-token", cfg.Auth.Token)
	assert.Equal(t, "work_schema", cfg.Storage.Postgres.Schema)
	assert.Empty(t, cfg.AutostartIdleTimeout)
	assert.Equal(t, personalHome, os.Getenv("KATA_HOME"))
}

func TestResolveLocalProfileStorageIgnoresParentDatabase(t *testing.T) {
	personalHome := t.TempDir()
	workHome := t.TempDir()
	t.Setenv("KATA_HOME", personalHome)
	t.Setenv("KATA_DB", filepath.Join(personalHome, "kata.db"))
	t.Setenv("KATA_DSN", "postgres://operator:personal-secret@daemon.example/personal_store")
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{
		Name: "work", Local: true, Home: workHome, InstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ01",
	})
	require.NoError(t, err)
	assert.Equal(t, workHome, profile.Home)
	assert.Equal(t, filepath.Join(workHome, "kata.db"), profile.DSN)
	assert.Equal(t, "01HZZZZZZZZZZZZZZZZZZZZZ01", profile.InstanceUID)
	_, err = os.Stat(profile.DSN)
	assert.ErrorIs(t, err, os.ErrNotExist, "configuration inspection must not create a database")
}

func TestResolveLocalProfileRejectsAmbiguousStorage(t *testing.T) {
	for _, tc := range []struct {
		name, dsn string
	}{
		{"relative sqlite", "work.db"},
		{"relative sqlite scheme", "sqlite://work.db"},
		{"missing postgres port", "postgres://operator:profile-secret@127.0.0.1/work_store"},
		{"missing postgres user", "postgres://127.0.0.1:5432/work_store"},
		{"missing postgres database", "postgres://operator:profile-secret@127.0.0.1:5432/"},
		{"postgres service", "postgres://operator:profile-secret@127.0.0.1:5432/work_store?service=work"},
		{"postgres host override", "postgres://operator:profile-secret@127.0.0.1:5432/work_store?host=daemon.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			body := fmt.Sprintf("[storage]\ndsn = %q\n", tc.dsn)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))
			_, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{
				Name: "work", Local: true, Home: home, InstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ01",
			})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "profile-secret")
		})
	}
}

func TestResolveLocalProfilePostgresKeepsExplicitTargetAndSchema(t *testing.T) {
	for _, key := range []string{"PGSERVICE", "PGPASSWORD", "PGPASSFILE", "PGSSLCERT", "PGSSLKEY", "PGSSLROOTCERT"} {
		t.Setenv(key, "")
	}
	t.Setenv("PGHOST", "wrong.example")
	t.Setenv("PGPORT", "6543")
	t.Setenv("PGDATABASE", "personal_store")
	t.Setenv("PGUSER", "personal_operator")
	t.Setenv("PGSSLMODE", "invalid")
	t.Setenv("KATA_POSTGRES_SCHEMA", "personal_schema")
	home := t.TempDir()
	dsn := "postgres://operator:profile-secret@127.0.0.1:5432/work_store?sslmode=disable" //nolint:gosec // Synthetic credential verifies namespace redaction.
	body := fmt.Sprintf("[storage]\ndsn = %q\n[storage.postgres]\nschema = \"work_schema\"\n", dsn)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{
		Name: "work", Local: true, Home: home, InstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ01",
	})
	require.NoError(t, err)
	assert.Equal(t, dsn, profile.DSN)
	assert.Equal(t, "work_schema", profile.Config.Storage.Postgres.Schema)
	u, err := url.Parse(profile.DSN)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:5432", u.Host)
	assert.Equal(t, "/work_store", u.Path)
	// The daemon's existing clean-environment hash must locate the same runtime.
	for _, key := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGSSLMODE"} {
		t.Setenv(key, "")
	}
	assert.Equal(t, config.StorageHash(dsn, "work_schema"), profile.StorageID)
}

func TestResolveLocalProfileRejectsAmbientPostgresCredentialsOrService(t *testing.T) {
	for _, key := range []string{"PGPASSWORD", "PGSERVICE", "PGSSLMODE"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("PGPASSWORD", "")
			t.Setenv("PGSERVICE", "")
			t.Setenv("PGSSLMODE", "")
			t.Setenv(key, "personal-secret")
			home := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[storage]
dsn = "postgres://operator@127.0.0.1:5432/work_store"
`), 0o600))
			_, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{
				Name: "work", Local: true, Home: home, InstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ01",
			})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "personal-secret")
		})
	}
}

func TestMergeLocalProfileReplacesRoutingAsOneTarget(t *testing.T) {
	base := &config.ProjectConfig{Server: config.ServerConfig{URL: "https://daemon.example"}}
	local := &config.ProjectConfig{Server: config.ServerConfig{Daemon: "work"}}
	merged := config.MergeLocal(base, local)
	assert.Equal(t, config.ServerConfig{Daemon: "work"}, merged.Server)
	assert.Equal(t, "https://daemon.example", base.Server.URL)
}

func TestLocalProfileEnvironmentIsolatesDaemonOverrides(t *testing.T) {
	for key, value := range map[string]string{
		"KATA_HOME": "personal-home", "KATA_DB": "personal.db", "KATA_DSN": "personal-dsn", "KATA_SERVER": "http://127.0.0.1:7790", "KATA_AUTH_TOKEN": "personal-token", "KATA_POSTGRES_SCHEMA": "personal_schema", "PGPASSWORD": "personal-secret", "HTTP_PROXY": "http://proxy.example", "HTTPS_PROXY": "http://proxy.example", "PORT": "8888", "WORK_TOKEN": "work-token", "PATH": os.Getenv("PATH"),
	} {
		t.Setenv(key, value)
	}
	profile := config.LocalProfileConfig{Home: t.TempDir(), Config: &config.DaemonConfig{Search: config.SearchConfig{Embeddings: config.EmbeddingsConfig{APIKeyEnv: "WORK_TOKEN"}}}}
	env, err := config.LocalProfileEnvironment(profile, true)
	require.NoError(t, err)
	values := map[string]string{}
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		values[key] = value
	}
	assert.Equal(t, profile.Home, values["KATA_HOME"])
	assert.Equal(t, "work-token", values["WORK_TOKEN"])
	assert.Equal(t, os.Getenv("PATH"), values["PATH"])
	assert.Equal(t, "1", values["KATA_AUTOSTART"])
	for _, key := range []string{"KATA_DB", "KATA_DSN", "KATA_SERVER", "KATA_AUTH_TOKEN", "KATA_POSTGRES_SCHEMA", "PGPASSWORD", "HTTP_PROXY", "HTTPS_PROXY", "PORT"} {
		assert.NotContains(t, values, key)
	}
	assert.Equal(t, "personal-token", os.Getenv("KATA_AUTH_TOKEN"))
}

func TestLocalProfileEnvironmentPreservesTelemetryOptOut(t *testing.T) {
	t.Setenv("KATA_TELEMETRY_ENABLED", "0")
	profile := config.LocalProfileConfig{Home: t.TempDir()}

	env, err := config.LocalProfileEnvironment(profile, true)

	require.NoError(t, err)
	assert.Contains(t, env, "KATA_TELEMETRY_ENABLED=0")
}

func TestLocalProfileEnvironmentIncludesDefaultGitHubTokenEnv(t *testing.T) {
	t.Setenv("KATA_GITHUB_TOKEN", "profile-github-token")
	profile := config.LocalProfileConfig{Home: t.TempDir(), Config: &config.DaemonConfig{}}

	env, err := config.LocalProfileEnvironment(profile, true)

	require.NoError(t, err)
	assert.Contains(t, env, "KATA_GITHUB_TOKEN=profile-github-token")
}

func TestLocalProfileEnvironmentIncludesPlaneToken(t *testing.T) {
	t.Setenv("KATA_PLANE_TOKEN", "default-plane-token")
	t.Setenv("EXAMPLE_PLANE_TOKEN", "custom-plane-token")
	for _, tc := range []struct {
		name, config, want string
	}{
		{"default", "", "KATA_PLANE_TOKEN=default-plane-token"},
		{"custom", "[plane_sync]\ntoken_env = \"EXAMPLE_PLANE_TOKEN\"\n", "EXAMPLE_PLANE_TOKEN=custom-plane-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.config != "" {
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.config), 0o600))
			}
			profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{
				Name: "example-profile", Local: true, Home: home, InstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ01",
			})
			require.NoError(t, err)

			env, err := config.LocalProfileEnvironment(profile, true)

			require.NoError(t, err)
			assert.True(t, slices.Contains(env, tc.want), "child environment must include the selected Plane token")
		})
	}
}

func TestLocalProfileEnvironmentIncludesConfiguredNotionAndPlaneTokenEnvs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		notionEnv string
		planeEnv  string
	}{
		{name: "defaults"},
		{name: "custom", notionEnv: "EXAMPLE_NOTION_TOKEN", planeEnv: "EXAMPLE_PLANE_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notionSync, err := config.NormalizeNotionSyncConfig(config.NotionSyncConfig{TokenEnv: tc.notionEnv})
			require.NoError(t, err)
			planeSync, err := config.NormalizePlaneSyncConfig(config.PlaneSyncConfig{TokenEnv: tc.planeEnv})
			require.NoError(t, err)
			t.Setenv(notionSync.TokenEnv, "example-notion-token")
			t.Setenv(planeSync.TokenEnv, "example-plane-token")
			profile := config.LocalProfileConfig{Home: t.TempDir(), Config: &config.DaemonConfig{NotionSync: notionSync, PlaneSync: planeSync}}

			env, err := config.LocalProfileEnvironment(profile, true)

			require.NoError(t, err)
			assert.Contains(t, env, notionSync.TokenEnv+"=example-notion-token")
			assert.Contains(t, env, planeSync.TokenEnv+"=example-plane-token")
		})
	}
}

func TestLocalProfileEnvironmentRejectsReservedCredentialReferences(t *testing.T) {
	for _, key := range []string{"KATA_AUTH_TOKEN", "KATA_SERVER", "KATA_GITHUB_SYNC_ALLOWED_HOSTS", "PGPASSWORD", "HTTPS_PROXY", "PORT", "PATH"} {
		t.Run(key, func(t *testing.T) {
			profile := config.LocalProfileConfig{Home: t.TempDir(), Config: &config.DaemonConfig{Search: config.SearchConfig{Embeddings: config.EmbeddingsConfig{APIKeyEnv: key}}}}
			_, err := config.LocalProfileEnvironment(profile, false)
			require.Error(t, err)
		})
	}
}

func TestLocalProfileEnvironmentPinsSelectedStorageSnapshot(t *testing.T) {
	t.Setenv("KATA_DSN", "personal-store")
	t.Setenv("KATA_POSTGRES_SCHEMA", "personal_schema")
	//nolint:gosec // Synthetic credential verifies explicit child storage settings.
	profile := config.LocalProfileConfig{Home: t.TempDir(), DSN: "postgres://operator:work-secret@127.0.0.1:5432/work_store?sslmode=disable", Config: &config.DaemonConfig{Storage: config.StorageConfig{Postgres: config.PostgresStorageConfig{Schema: "work_schema"}}}}
	env, err := config.LocalProfileEnvironment(profile, true)
	require.NoError(t, err)
	assert.Contains(t, env, "KATA_DSN="+profile.DSN)
	assert.Contains(t, env, "KATA_POSTGRES_SCHEMA=work_schema")
	assert.NotContains(t, env, "KATA_DSN=personal-store")
	assert.NotContains(t, env, "KATA_POSTGRES_SCHEMA=personal_schema")
}

func TestLocalProfileEnvironmentIncludesDocumentedHubCredentials(t *testing.T) {
	for _, key := range []string{"KATA_TEAM_HUB_TOKEN", "KATA_SHARED_TOKEN"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "hub-token")
			profile := config.LocalProfileConfig{Home: t.TempDir(), Config: &config.DaemonConfig{
				Daemons: []config.CatalogDaemonConfig{{Name: "work-hub", URL: "https://daemon.example", TokenEnv: key}},
			}}
			env, err := config.LocalProfileEnvironment(profile, true)
			require.NoError(t, err)
			assert.Contains(t, env, key+"=hub-token")
		})
	}
}

func TestLocalProfileEnvironmentTrustsConfiguredCertificateAuthority(t *testing.T) {
	if target := os.Getenv("PROFILE_TLS_PROBE"); target != "" {
		response, err := http.Get(target) //nolint:gosec // Parent supplies only its loopback TLS fixture.
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		require.Equal(t, http.StatusNoContent, response.StatusCode)
		return
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("SSL_CERT_FILE and SSL_CERT_DIR configure Unix system roots")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	certPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
	for _, key := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("SSL_CERT_FILE", "")
			t.Setenv("SSL_CERT_DIR", "")
			value := certPath
			if key == "SSL_CERT_DIR" {
				value = filepath.Dir(certPath)
			}
			t.Setenv(key, value)
			env, err := config.LocalProfileEnvironment(config.LocalProfileConfig{Home: t.TempDir()}, true)
			require.NoError(t, err)
			//nolint:gosec // Relaunch only this test executable under the selected profile environment.
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLocalProfileEnvironmentTrustsConfiguredCertificateAuthority$")
			child.Env = append(env, "PROFILE_TLS_PROBE="+server.URL)
			output, err := child.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}

// Contract: a local-profile daemon receives the signing secrets named by its
// own home's hub keys and saved spoke credentials, not the invoking home's.
func TestLocalProfileEnvironmentIncludesFederationSigningKeyEnvs(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("EXAMPLE_HUB_SIGNING_KEY", "hub-signing-secret")
	t.Setenv("EXAMPLE_SPOKE_SIGNING_KEY", "spoke-signing-secret")
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[federation.signing]
external_url = "https://daemon.example/federation"
[[federation.signing.key]]
key_id = "hub-key"
key_env = "EXAMPLE_HUB_SIGNING_KEY"
enrollment_id = 1
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "credentials.toml"), []byte(`
[projects.01HZNQ7VFPK1XGD8R5MABCD4EX]
hub_url = "https://daemon.example/federation"
hub_project_id = 1
token = "enrollment"
[projects.01HZNQ7VFPK1XGD8R5MABCD4EX.signing]
key_id = "spoke-key"
key_env = "EXAMPLE_SPOKE_SIGNING_KEY"
`), 0o600))
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{
		Name: "example-profile", Local: true, Home: home, InstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ01",
	})
	require.NoError(t, err)

	env, err := config.LocalProfileEnvironment(profile, true)

	require.NoError(t, err)
	assert.Contains(t, env, "EXAMPLE_HUB_SIGNING_KEY=hub-signing-secret")
	assert.Contains(t, env, "EXAMPLE_SPOKE_SIGNING_KEY=spoke-signing-secret")
}

// Contract: credential references that share one environment variable are
// copied once instead of being mistaken for reserved OS variables.
func TestLocalProfileEnvironmentAllowsSharedCredentialReferences(t *testing.T) {
	t.Setenv("EXAMPLE_SHARED_SIGNING_KEY", "shared-signing-secret")
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "credentials.toml"), []byte(`
[projects.01HZNQ7VFPK1XGD8R5MABCD4EX]
hub_url = "https://daemon.example"
[projects.01HZNQ7VFPK1XGD8R5MABCD4EX.signing]
key_id = "key-a"
key_env = "EXAMPLE_SHARED_SIGNING_KEY"
[projects.01HZNQ7VFPK1XGD8R5MABCD4EY]
hub_url = "https://daemon.example"
[projects.01HZNQ7VFPK1XGD8R5MABCD4EY.signing]
key_id = "key-b"
key_env = "EXAMPLE_SHARED_SIGNING_KEY"
`), 0o600))
	profile := config.LocalProfileConfig{Home: home, Config: &config.DaemonConfig{}}

	env, err := config.LocalProfileEnvironment(profile, true)

	require.NoError(t, err)
	assert.Contains(t, env, "EXAMPLE_SHARED_SIGNING_KEY=shared-signing-secret")
	_, err = config.LocalProfileEnvironment(config.LocalProfileConfig{Home: t.TempDir(), Config: &config.DaemonConfig{
		Search: config.SearchConfig{Embeddings: config.EmbeddingsConfig{APIKeyEnv: "PATH"}},
	}}, true)
	require.Error(t, err, "an OS allowlist variable remains reserved")
}

func TestLocalProfileEnvironmentIncludesLinearToken(t *testing.T) {
	t.Setenv("KATA_LINEAR_TOKEN", "default-linear-credential")
	t.Setenv("EXAMPLE_LINEAR_TOKEN", "custom-linear-credential")
	for _, tc := range []struct {
		name, config, want, excluded string
	}{
		{"default", "", "KATA_LINEAR_TOKEN=default-linear-credential", "EXAMPLE_LINEAR_TOKEN=custom-linear-credential"},
		{"custom", "[linear_sync]\ntoken_env = \"EXAMPLE_LINEAR_TOKEN\"\n", "EXAMPLE_LINEAR_TOKEN=custom-linear-credential", "KATA_LINEAR_TOKEN=default-linear-credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.config != "" {
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.config), 0o600))
			}
			profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{
				Name: "example-profile", Local: true, Home: home, InstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ01",
			})
			require.NoError(t, err)

			env, err := config.LocalProfileEnvironment(profile, true)

			require.NoError(t, err)
			assert.True(t, slices.Contains(env, tc.want), "child environment must include the selected Linear token")
			assert.False(t, slices.Contains(env, tc.excluded), "unselected Linear credentials must remain outside the child environment")
		})
	}
}
