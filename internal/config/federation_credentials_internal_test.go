package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteFederationCredentialPartialTempWriteLeavesOldFileUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	const (
		localUID = "01HZNQ7VFPK1XGD8R5MABCD4EA"
		otherUID = "01HZNQ7VFPK1XGD8R5MABCD4EY"
	)
	manual := FederationCredential{
		HubURL: "https://hub.example", HubProjectID: 42,
		Token: "token-a", Capabilities: "claim,pull,push",
		Actor: "user-a",
	}
	other := FederationCredential{
		HubURL: "https://other.example", HubProjectID: 7,
		Token: "token-b", Capabilities: "pull",
		Actor: "user-b",
	}
	require.NoError(t, WriteFederationCredential(localUID, manual))
	require.NoError(t, WriteFederationCredential(otherUID, other))
	path, err := FederationCredentialsPath()
	require.NoError(t, err)
	before, err := os.ReadFile(path) //nolint:gosec // path is the test's isolated KATA_HOME.
	require.NoError(t, err)

	originalWriter := writeFederationCredentialsTempFile
	writeFederationCredentialsTempFile = func(file *os.File, data []byte) error {
		if _, writeErr := file.Write(data[:len(data)/2]); writeErr != nil {
			return writeErr
		}
		return errors.New("injected partial temporary write failure")
	}
	t.Cleanup(func() { writeFederationCredentialsTempFile = originalWriter })

	changed := manual
	changed.Actor = "identity-user"
	err = WriteFederationCredential(localUID, changed)
	require.ErrorContains(t, err, "injected partial temporary write failure")

	after, err := os.ReadFile(path) //nolint:gosec // path is the test's isolated KATA_HOME.
	require.NoError(t, err)
	assert.Equal(t, before, after)
	credentials, err := ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, manual, credentials.Projects[localUID])
	assert.Equal(t, other, credentials.Projects[otherUID])
	assertNoFederationCredentialTempFiles(t, home)
}

func TestWriteFederationCredentialRenameFailureLeavesOldFileUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	const projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EA"
	original := FederationCredential{
		HubURL: "https://hub.example", HubProjectID: 42,
		Token: "token-a", Capabilities: "claim,pull,push",
		Actor: "user-a",
	}
	require.NoError(t, WriteFederationCredential(projectUID, original))
	path, err := FederationCredentialsPath()
	require.NoError(t, err)
	before, err := os.ReadFile(path) //nolint:gosec // path is the test's isolated KATA_HOME.
	require.NoError(t, err)

	originalRename := renameFederationCredentialsFile
	renameFederationCredentialsFile = func(string, string) error {
		return errors.New("injected credential rename failure")
	}
	t.Cleanup(func() { renameFederationCredentialsFile = originalRename })

	changed := original
	changed.Actor = "identity-user"
	err = WriteFederationCredential(projectUID, changed)
	require.ErrorContains(t, err, "injected credential rename failure")

	after, err := os.ReadFile(path) //nolint:gosec // path is the test's isolated KATA_HOME.
	require.NoError(t, err)
	assert.Equal(t, before, after)
	credentials, err := ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, original, credentials.Projects[projectUID])
	assertNoFederationCredentialTempFiles(t, home)
}

func assertNoFederationCredentialTempFiles(t *testing.T, home string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, ".credentials.toml.tmp-*"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestReplaceFederationCredentialSupportsManualAndManagedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current FederationCredential
	}{
		{
			name: "manual",
			current: FederationCredential{
				HubURL: "http://192.0.2.10:7777", HubProjectID: 42,
				Token: "token-a", Capabilities: "claim,pull,push", Actor: "user-a",
				AllowInsecure: true,
			},
		},
		{
			name: "config managed",
			current: FederationCredential{
				HubURL: "http://192.0.2.10:7777", HubProjectID: 42,
				Token: "token-a", Capabilities: "claim,pull,push", Actor: "user-a",
				AllowInsecure: true, ManagedByConfig: true, HubCatalog: "primary-hub",
				HubProjectName: "hub-project", RequestedActor: "requested-user",
				SpokeProjectName: "spoke-project",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			const projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
			replacement := tc.current
			replacement.HubURL = "https://hub.example"
			replacement.AllowInsecure = false
			require.NoError(t, WriteFederationCredential(projectUID, tc.current))

			err := ReplaceFederationCredential(FederationCredentialReplacement{
				ProjectUID: projectUID, Expected: tc.current, Replacement: replacement,
			})

			require.NoError(t, err)
			credentials, readErr := ReadFederationCredentials()
			require.NoError(t, readErr)
			assert.Equal(t, replacement, credentials.Projects[projectUID])
		})
	}
}

func TestReplaceFederationCredentialExactTargetDoesNotRewriteFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	const projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	target := FederationCredential{
		HubURL: "https://hub.example", HubProjectID: 42,
		Token: "token-a", Capabilities: "claim,pull,push", Actor: "user-a",
	}
	require.NoError(t, WriteFederationCredential(projectUID, target))
	path, err := FederationCredentialsPath()
	require.NoError(t, err)
	before, err := os.ReadFile(path) //nolint:gosec // path is the test's isolated KATA_HOME.
	require.NoError(t, err)

	writes := 0
	renames := 0
	originalWriter := writeFederationCredentialsTempFile
	originalRename := renameFederationCredentialsFile
	writeFederationCredentialsTempFile = func(*os.File, []byte) error {
		writes++
		return nil
	}
	renameFederationCredentialsFile = func(string, string) error {
		renames++
		return nil
	}
	t.Cleanup(func() {
		writeFederationCredentialsTempFile = originalWriter
		renameFederationCredentialsFile = originalRename
	})

	err = ReplaceFederationCredential(FederationCredentialReplacement{
		ProjectUID:  projectUID,
		Expected:    FederationCredential{HubURL: "http://192.0.2.10:7777", Token: "old"},
		Replacement: target,
	})

	require.NoError(t, err)
	assert.Zero(t, writes)
	assert.Zero(t, renames)
	after, readErr := os.ReadFile(path) //nolint:gosec // path is the test's isolated KATA_HOME.
	require.NoError(t, readErr)
	assert.Equal(t, before, after)
}

func TestReplaceFederationCredentialRejectsMissingOrChangedSource(t *testing.T) {
	const projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	current := FederationCredential{
		HubURL: "http://192.0.2.10:7777", HubProjectID: 42,
		Token: "token-a", AllowInsecure: true,
	}
	target := current
	target.HubURL = "https://hub.example"
	target.AllowInsecure = false

	for _, tc := range []struct {
		name       string
		projectUID string
		stored     *FederationCredential
	}{
		{name: "empty project UID"},
		{name: "missing credential", projectUID: projectUID},
		{name: "changed credential", projectUID: projectUID, stored: func() *FederationCredential {
			changed := current
			changed.Token = "concurrent-token"
			return &changed
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			if tc.stored != nil {
				require.NoError(t, WriteFederationCredential(projectUID, *tc.stored))
			}
			path := filepath.Join(home, "credentials.toml")
			before, _ := os.ReadFile(path) //nolint:gosec // path is the test's isolated KATA_HOME.

			err := ReplaceFederationCredential(FederationCredentialReplacement{
				ProjectUID: tc.projectUID, Expected: current, Replacement: target,
			})

			require.ErrorIs(t, err, ErrFederationCredentialConflict)
			after, _ := os.ReadFile(path) //nolint:gosec // path is the test's isolated KATA_HOME.
			assert.Equal(t, before, after)
		})
	}
}

func TestReplaceFederationCredentialWriteFailuresLeaveSourceUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(t *testing.T)
	}{
		{
			name: "partial temporary write",
			inject: func(t *testing.T) {
				original := writeFederationCredentialsTempFile
				writeFederationCredentialsTempFile = func(file *os.File, data []byte) error {
					_, err := file.Write(data[:len(data)/2])
					require.NoError(t, err)
					return errors.New("injected replacement write failure")
				}
				t.Cleanup(func() { writeFederationCredentialsTempFile = original })
			},
		},
		{
			name: "rename",
			inject: func(t *testing.T) {
				original := renameFederationCredentialsFile
				renameFederationCredentialsFile = func(string, string) error {
					return errors.New("injected replacement rename failure")
				}
				t.Cleanup(func() { renameFederationCredentialsFile = original })
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			const projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
			current := FederationCredential{
				HubURL: "http://192.0.2.10:7777", HubProjectID: 42,
				Token: "token-a", Capabilities: "claim,pull,push", Actor: "user-a",
				AllowInsecure: true,
			}
			target := current
			target.HubURL = "https://hub.example"
			target.AllowInsecure = false
			require.NoError(t, WriteFederationCredential(projectUID, current))
			path, err := FederationCredentialsPath()
			require.NoError(t, err)
			before, err := os.ReadFile(path) //nolint:gosec // path is the test's isolated KATA_HOME.
			require.NoError(t, err)
			tc.inject(t)

			err = ReplaceFederationCredential(FederationCredentialReplacement{
				ProjectUID: projectUID, Expected: current, Replacement: target,
			})

			require.Error(t, err)
			after, readErr := os.ReadFile(path) //nolint:gosec // path is the test's isolated KATA_HOME.
			require.NoError(t, readErr)
			assert.Equal(t, before, after)
			credentials, readErr := ReadFederationCredentials()
			require.NoError(t, readErr)
			assert.Equal(t, current, credentials.Projects[projectUID])
			assertNoFederationCredentialTempFiles(t, home)
		})
	}
}

func TestDefaultFederationCredentialStoreSupportsExactReplacement(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	const projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	current := FederationCredential{
		HubURL: "http://192.0.2.10:7777", HubProjectID: 42,
		Token: "token-a", AllowInsecure: true,
	}
	target := current
	target.HubURL = "https://hub.example"
	target.AllowInsecure = false
	store := DefaultFederationCredentialStore()
	require.NoError(t, store.StoreFederationCredential(ctx, projectUID, current))
	replacer, ok := store.(FederationCredentialReplacer)
	require.True(t, ok)
	require.NoError(t, replacer.ReplaceFederationCredential(ctx, FederationCredentialReplacement{
		ProjectUID: projectUID, Expected: current, Replacement: target,
	}))
	stored, found, err := store.FederationCredential(ctx, projectUID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, target, stored)
}
