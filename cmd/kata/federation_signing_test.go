package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationSigningCLIUsesSelectedDaemon(t *testing.T) {
	for _, selector := range []string{"server", "catalog"} {
		t.Run(selector, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("daemon-admin"))
			t.Setenv("TEST_DAEMON_SIGNING_KEY", strings.Repeat("k", 64))
			uid := "01HZNQ7VFPK1XGD8R5MABCD4EX"
			credential := config.FederationCredential{
				HubURL: "https://hub.example/mount", HubProjectID: 1,
				Token: "daemon-enrollment", Actor: "example-actor",
			}
			require.NoError(t, config.WriteFederationCredential(uid, credential))
			cliHome := t.TempDir()
			args := []string{"federation", "signing", "configure", "--project-uid", uid,
				"--key-id", "key-a", "--key-env", "TEST_DAEMON_SIGNING_KEY"}
			if selector == "catalog" {
				require.NoError(t, os.WriteFile(filepath.Join(cliHome, "config.toml"),
					fmt.Appendf(nil, "[[daemon]]\nname = \"spoke-daemon\"\nurl = %q\ntoken = \"daemon-admin\"\n", env.URL), 0600))
				args = append([]string{"--daemon", "spoke-daemon"}, args...)
			}
			//nolint:gosec // Runs this test binary with fixed fixture arguments.
			child := exec.CommandContext(t.Context(), os.Args[0],
				append([]string{"-test.run=^TestFederationSigningCLIHelperProcess$", "--"}, args...)...)
			child.Dir = cliHome
			for _, key := range []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "SYSTEMROOT"} {
				if value, ok := os.LookupEnv(key); ok {
					child.Env = append(child.Env, key+"="+value)
				}
			}
			child.Env = append(child.Env, "KATA_SIGNING_CLI_HELPER=1", "KATA_HOME="+cliHome,
				"KATA_DB="+filepath.Join(cliHome, "kata.db"), "GIT_CONFIG_NOSYSTEM=1",
				"GIT_CONFIG_GLOBAL="+filepath.Join(cliHome, "gitconfig"))
			if selector == "server" {
				child.Env = append(child.Env, "KATA_SERVER="+env.URL, "KATA_AUTH_TOKEN=daemon-admin")
			}
			output, err := child.CombinedOutput()
			require.NoError(t, err, "%s", output)
			all, err := config.ReadFederationCredentials()
			require.NoError(t, err)
			expected := credential
			expected.Signing = &federationsigning.Source{
				KeyID: "key-a", KeyEnv: "TEST_DAEMON_SIGNING_KEY", HubURL: credential.HubURL,
			}
			require.Equal(t, expected, all.Projects[uid])
			raw, err := fs.ReadFile(os.DirFS(env.Home), "credentials.toml")
			require.NoError(t, err)
			require.NotContains(t, string(raw), strings.Repeat("k", 64))
		})
	}
}

func TestFederationSigningCLIHelperProcess(t *testing.T) {
	if os.Getenv("KATA_SIGNING_CLI_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	uid := "01HZNQ7VFPK1XGD8R5MABCD4EX"
	credential := config.FederationCredential{
		HubURL: "https://local-hub.example", HubProjectID: 99, Token: "local-enrollment",
	}
	require.NoError(t, config.WriteFederationCredential(uid, credential))
	require.Empty(t, os.Getenv("TEST_DAEMON_SIGNING_KEY"))
	separator := slices.Index(os.Args, "--")
	require.Positive(t, separator)
	_, err := runCmdOutput(t, nil, os.Args[separator+1:]...)
	require.NoError(t, err)
	all, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	require.Equal(t, credential, all.Projects[uid], "the CLI's local enrollment must not change")
}

func TestFederationSigningReplayStateNeverReplacesExistingState(t *testing.T) {
	state := filepath.Join(t.TempDir(), "federation-signing-replay.state")
	cmd := federationSigningCmd()
	cmd.SetArgs([]string{"init-replay", "--state-file", state})
	require.NoError(t, cmd.Execute())
	cmd = federationSigningCmd()
	cmd.SetArgs([]string{"init-replay", "--state-file", state})
	require.Error(t, cmd.Execute())
}

func TestFederationSigningReplayStateRequiresAbsolutePath(t *testing.T) {
	t.Chdir(t.TempDir())
	cmd := federationSigningCmd()
	cmd.SetArgs([]string{"init-replay", "--state-file", "relative.state"})

	err := cmd.Execute()
	require.ErrorContains(t, err, "absolute")
	_, err = os.Stat("relative.state")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestJoinSigningFlagsAndSourceSelection(t *testing.T) {
	t.Setenv("TEST_JOIN_SIGNING_KEY", strings.Repeat("k", 64))
	cmd := federationJoinCmd()
	require.NotNil(t, cmd.Flags().Lookup("signing-key-id"))
	require.NotNil(t, cmd.Flags().Lookup("signing-key-env"))
	s, err := signingSource("https://hub.example/mount", "key-a", "", "TEST_JOIN_SIGNING_KEY")
	require.NoError(t, err)
	require.Equal(t, "https://hub.example/mount", s.HubURL)
	s, err = signingSource("https://HUB.EXAMPLE:443/mount", "key-a", "", "TEST_JOIN_SIGNING_KEY")
	require.NoError(t, err)
	require.Equal(t, "https://hub.example/mount", s.HubURL)
	keyFile := filepath.Join(t.TempDir(), "signing.key")
	require.NoError(t, os.WriteFile(keyFile, []byte(strings.Repeat("k", 64)), 0600))
	s, err = signingSource("https://hub.example", "key-a", keyFile, "")
	require.NoError(t, err)
	require.Equal(t, keyFile, s.KeyFile)
	require.Empty(t, s.KeyEnv)
}

func TestSigningSourceResolvesRelativeKeyFileForDaemonWorkingDirectory(t *testing.T) {
	cliDir := t.TempDir()
	daemonDir := t.TempDir()
	key := []byte(strings.Repeat("k", 64))
	require.NoError(t, os.WriteFile(filepath.Join(cliDir, "signing.key"), key, 0600))
	t.Chdir(cliDir)

	source, err := signingSource("https://hub.example", "key-a", "signing.key", "")
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(source.KeyFile))

	t.Chdir(daemonDir)
	loaded, err := source.Load()
	require.NoError(t, err)
	require.Equal(t, key, loaded)
}
