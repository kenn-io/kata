package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestReadFederationCredentialsMissingReturnsEmpty(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())

	creds, err := config.ReadFederationCredentials()

	require.NoError(t, err)
	require.NotNil(t, creds)
	assert.NotNil(t, creds.Projects)
	assert.Empty(t, creds.Projects)
}

func TestWriteFederationCredentialRoundTrips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)

	require.NoError(t, config.WriteFederationCredential("01HZNQ7VFPK1XGD8R5MABCD4EX",
		config.FederationCredential{
			HubURL:        "http://127.0.0.1:7373",
			HubProjectID:  42,
			Token:         "secret-token",
			Capabilities:  "pull,push,claim",
			Actor:         "wesm",
			AllowInsecure: true,
		}))

	path := filepath.Join(home, "credentials.toml")
	info, err := os.Stat(path)
	require.NoError(t, err)
	// Unix permission bits are not meaningful on Windows (files report 0666/
	// 0444 by the read-only bit); the 0600 intent is enforced via ACLs there.
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	got := creds.Projects["01HZNQ7VFPK1XGD8R5MABCD4EX"]
	assert.Equal(t, "http://127.0.0.1:7373", got.HubURL)
	assert.Equal(t, int64(42), got.HubProjectID)
	assert.Equal(t, "secret-token", got.Token)
	assert.Equal(t, "pull,push,claim", got.Capabilities)
	assert.Equal(t, "wesm", got.Actor)
	assert.True(t, got.AllowInsecure)
}

func TestReadFederationCredentialWithoutCapabilitiesDefaultsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	path := filepath.Join(home, "credentials.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[projects."01HZNQ7VFPK1XGD8R5MABCD4EX"]
hub_url = "http://127.0.0.1:7373"
hub_project_id = 42
token = "secret-token"
`), 0o600))

	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	got := creds.Projects["01HZNQ7VFPK1XGD8R5MABCD4EX"]
	assert.Equal(t, "http://127.0.0.1:7373", got.HubURL)
	assert.Equal(t, int64(42), got.HubProjectID)
	assert.Equal(t, "secret-token", got.Token)
	assert.Equal(t, "", got.Capabilities)
}

func TestWriteFederationCredentialTightensExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file-mode tightening is not meaningful on Windows (ACL-based)")
	}
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	path := filepath.Join(home, "credentials.toml")
	require.NoError(t, os.WriteFile(path, []byte(""), 0o600))
	require.NoError(t, os.Chmod(path, 0o644)) //nolint:gosec // Intentionally simulates a preexisting loose credentials file.

	require.NoError(t, config.WriteFederationCredential("01HZNQ7VFPK1XGD8R5MABCD4EX",
		config.FederationCredential{
			HubURL:       "http://127.0.0.1:7373",
			HubProjectID: 42,
			Token:        "secret-token",
		}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestFederationCredentialMetadataForPresentCredentialRedactsToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, config.WriteFederationCredential("01HZNQ7VFPK1XGD8R5MABCD4EX",
		config.FederationCredential{
			HubURL:        "http://hub.internal:7777",
			HubProjectID:  42,
			Token:         "secret-token",
			Capabilities:  "claim,pull,push",
			Actor:         "wesm",
			AllowInsecure: true,
		}))

	got := config.FederationCredentialMetadataFor("01HZNQ7VFPK1XGD8R5MABCD4EX")

	assert.Equal(t, "present", got.Status)
	assert.Equal(t, "http://hub.internal:7777", got.HubURL)
	assert.Equal(t, int64(42), got.HubProjectID)
	assert.Equal(t, "claim,pull,push", got.Capabilities)
	assert.Equal(t, "wesm", got.Actor)
	assert.True(t, got.AllowInsecure)
}

func TestFederationCredentialMetadataForMissingCredential(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())

	got := config.FederationCredentialMetadataFor("01HZNQ7VFPK1XGD8R5MABCD4EX")

	assert.Equal(t, "missing", got.Status)
	assert.False(t, got.AllowInsecure)
}

func TestFederationCredentialMetadataForUnreadableCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "credentials.toml"), []byte("[projects."), 0o600))

	got := config.FederationCredentialMetadataFor("01HZNQ7VFPK1XGD8R5MABCD4EX")

	assert.Equal(t, "unreadable", got.Status)
	assert.False(t, got.AllowInsecure)
}

func TestDeleteFederationCredentialIdempotent(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	uid := "01KQJF75QFKWXHASB1QK74ZACZ"
	other := "01KT4E0PJ4FRW0T0QB86EMZ1SN"
	if err := config.WriteFederationCredential(uid, config.FederationCredential{HubURL: "http://hub.example", HubProjectID: 1, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteFederationCredential(other, config.FederationCredential{HubURL: "http://hub.example", HubProjectID: 2, Token: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := config.DeleteFederationCredential(uid); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := config.FederationCredentialMetadataFor(uid).Status; got != "missing" {
		t.Fatalf("want missing, got %q", got)
	}
	if got := config.FederationCredentialMetadataFor(other).Status; got != "present" {
		t.Fatalf("other credential should survive, got %q", got)
	}
	if err := config.DeleteFederationCredential(uid); err != nil {
		t.Fatalf("second delete should be nil, got %v", err)
	}
	t.Setenv("KATA_HOME", t.TempDir())
	if err := config.DeleteFederationCredential(uid); err != nil {
		t.Fatalf("delete with no file should be nil, got %v", err)
	}
}
