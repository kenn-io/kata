package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagedCredentialMutationsSerializeWholeReadModifyWrite(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	firstReplacementStarted := make(chan struct{})
	releaseFirstReplacement := make(chan struct{})
	defer func() {
		select {
		case <-releaseFirstReplacement:
		default:
			close(releaseFirstReplacement)
		}
	}()

	originalWriter := writeFederationCredentialsTempFile
	var writes sync.Once
	writeFederationCredentialsTempFile = func(file *os.File, data []byte) error {
		writes.Do(func() {
			close(firstReplacementStarted)
			<-releaseFirstReplacement
		})
		return originalWriter(file, data)
	}
	t.Cleanup(func() { writeFederationCredentialsTempFile = originalWriter })

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- ReserveManagedFederationCredential(
			FederationManagedCredentialReservation{
				ProjectUID: "01HUBPROJECT00000000000000",
				Credential: FederationCredential{
					Token: "first-token", ManagedByConfig: true,
					SpokeProjectName: "first-project",
				},
			})
	}()
	<-firstReplacementStarted

	secondAttemptStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondAttemptStarted)
		secondDone <- ReserveManagedFederationCredential(
			FederationManagedCredentialReservation{
				ProjectUID: "01HUBPROJECT00000000000001",
				Credential: FederationCredential{
					Token: "second-token", ManagedByConfig: true,
					SpokeProjectName: "second-project",
				},
			})
	}()
	<-secondAttemptStarted
	select {
	case err := <-secondDone:
		t.Fatalf("second managed mutation completed before first replacement released: %v", err)
	default:
	}

	close(releaseFirstReplacement)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	credentials, err := ReadFederationCredentials()
	require.NoError(t, err)
	assert.Contains(t, credentials.Projects, "01HUBPROJECT00000000000000")
	assert.Contains(t, credentials.Projects, "01HUBPROJECT00000000000001")
}

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
