package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/federationsigning"
)

func TestFederationSigningCLIUsesReferencesAndExactReplacement(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("TEST_CLI_SIGNING_KEY", strings.Repeat("k", 64))
	uid := "01HZNQ7VFPK1XGD8R5MABCD4EX"
	credential := config.FederationCredential{HubURL: "https://hub.example/mount", HubProjectID: 1, Token: "enrollment", Actor: "example-actor"}
	require.NoError(t, config.WriteFederationCredential(uid, credential))
	cmd := federationSigningCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"configure", "--project-uid", uid, "--key-id", "key-a", "--key-env", "TEST_CLI_SIGNING_KEY"})
	require.NoError(t, cmd.Execute())
	all, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	got := all.Projects[uid]
	require.Equal(t, credential.Token, got.Token)
	require.Equal(t, &federationsigning.Source{KeyID: "key-a", KeyEnv: "TEST_CLI_SIGNING_KEY", HubURL: credential.HubURL}, got.Signing)
	raw, err := fs.ReadFile(os.DirFS(home), "credentials.toml")
	require.NoError(t, err)
	require.NotContains(t, string(raw), strings.Repeat("k", 64))
	state := filepath.Join(home, "federation-signing-replay.state")
	cmd = federationSigningCmd()
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
