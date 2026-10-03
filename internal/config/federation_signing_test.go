package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/federationsigning"
)

func TestFederationSigningConfigAndCredentialRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("TEST_SIGNING_KEY", strings.Repeat("k", 64))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[federation.signing]
required = true
external_url = "https://hub.example/federation"
[[federation.signing.key]]
key_id = "key-a"
key_env = "TEST_SIGNING_KEY"
enrollment_id = 7
[federation.ingress]
enabled = true
`), 0600))
	c, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.True(t, c.Federation.Signing.Required)
	require.Equal(t, "127.0.0.1:0", c.Federation.Ingress.Listen)
	s := federationsigning.Source{KeyID: "key-a", KeyEnv: "TEST_SIGNING_KEY", HubURL: "https://hub.example/federation"}
	credential := config.FederationCredential{HubURL: s.HubURL, HubProjectID: 1, Token: "enrollment", Signing: &s}
	require.NoError(t, config.WriteFederationCredential("01EXAMPLEPROJECT0000000000", credential))
	all, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	require.Equal(t, credential, all.Projects["01EXAMPLEPROJECT0000000000"])
}

func TestFederationSigningInvalidPolicyFailsClosed(t *testing.T) {
	t.Setenv("TEST_SIGNING_KEY", strings.Repeat("k", 64))
	for _, body := range []string{
		`[federation.signing]
required = true`,
		`[federation.ingress]
enabled = true
listen = "0.0.0.0:7788"`,
	} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600))
			_, err := config.ReadDaemonConfig()
			require.Error(t, err)
		})
	}
}

func TestFederationSigningConfigReadDefersSecretValidationToDaemonVerifier(t *testing.T) {
	t.Setenv("TEST_DAEMON_SIGNING_KEY", "")
	t.Setenv("TEST_DUPLICATE_SIGNING_KEY", strings.Repeat("k", 64))
	for _, tc := range []struct {
		name string
		keys string
		want error
	}{
		{
			name: "missing secret",
			keys: `[[federation.signing.key]]
key_id = "key-a"
key_env = "TEST_DAEMON_SIGNING_KEY"
enrollment_id = 1`,
			want: federationsigning.ErrKeyRejected,
		},
		{
			name: "duplicate secrets",
			keys: `[[federation.signing.key]]
key_id = "key-a"
key_env = "TEST_DUPLICATE_SIGNING_KEY"
enrollment_id = 1
[[federation.signing.key]]
key_id = "key-b"
key_env = "TEST_DUPLICATE_SIGNING_KEY"
enrollment_id = 2`,
			want: federationsigning.ErrKey,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			body := "[federation.signing]\nexternal_url = \"https://hub.example\"\n" + tc.keys + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600))

			cfg, err := config.ReadDaemonConfigForHome(home)
			require.NoError(t, err)

			_, err = federationsigning.NewVerifier(
				cfg.Federation.Signing.ExternalURL,
				cfg.Federation.Signing.Keys,
				cfg.Federation.Signing.ReplayStateFile,
			)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestFederationSigningPartialConfigReportsMissingFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "required without external URL",
			body: `[federation.signing]
required = true`,
			want: "federation.signing.external_url is required when federation signing is configured",
		},
		{
			name: "external URL without keys",
			body: `[federation.signing]
external_url = "https://hub.example"`,
			want: "federation.signing.key entries are required when federation signing is configured",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.body), 0600))
			_, err := config.ReadDaemonConfigForHome(home)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestFederationSigningRotationPolicy(t *testing.T) {
	activeKey := func(id string, enrollmentID int64) string {
		return fmt.Sprintf(`[[federation.signing.key]]
key_id = %q
key_env = "TEST_SIGNING_KEY_%s"
enrollment_id = %d`, id, id, enrollmentID)
	}
	retiringKey := func(id string, enrollmentID int64, notAfter int64) string {
		return fmt.Sprintf(`[[federation.signing.key]]
key_id = %q
key_env = "TEST_SIGNING_KEY_%s"
enrollment_id = %d
not_after = %d`, id, id, enrollmentID, notAfter)
	}
	withinWindow := time.Now().Add(12 * time.Hour).Unix()
	outsideWindow := time.Now().Add(25 * time.Hour).Unix()

	for _, tc := range []struct {
		name    string
		keys    []string
		wantErr string
	}{
		{
			name:    "rejects two active keys for one enrollment",
			keys:    []string{activeKey("key-a", 1), activeKey("key-b", 1)},
			wantErr: "federation signing requires one active key and at most one retiring key per enrollment",
		},
		{
			name: "rejects more than one retiring key",
			keys: []string{
				activeKey("key-a", 1),
				retiringKey("key-b", 1, withinWindow),
				retiringKey("key-c", 1, withinWindow),
			},
			wantErr: "federation signing requires one active key and at most one retiring key per enrollment",
		},
		{
			name:    "rejects retirement beyond 24 hours",
			keys:    []string{activeKey("key-a", 1), retiringKey("key-b", 1, outsideWindow)},
			wantErr: "federation signing rotation overlap must end within 24 hours",
		},
		{
			name:    "rejects a retiring-only enrollment",
			keys:    []string{retiringKey("key-a", 1, withinWindow)},
			wantErr: "federation signing requires one active key and at most one retiring key per enrollment",
		},
		{
			name: "accepts one active key and one retiring key",
			keys: []string{activeKey("key-a", 1), retiringKey("key-b", 1, withinWindow)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			body := "[federation.signing]\nexternal_url = \"https://hub.example\"\n" + strings.Join(tc.keys, "\n") + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600))

			_, err := config.ReadDaemonConfigForHome(home)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}
