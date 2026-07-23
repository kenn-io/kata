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

type controlledFederationCredentialsLock struct {
	state       sync.Mutex
	held        bool
	waiters     []chan struct{}
	attempts    int
	attempted   chan struct{}
	initialize  func() error
	initialized chan error
	unlocked    chan federationCredentialsUnlock
}

type federationCredentialsUnlock struct {
	credentials *FederationCredentials
	err         error
}

func newControlledFederationCredentialsLock(
	initialize func() error,
) *controlledFederationCredentialsLock {
	return &controlledFederationCredentialsLock{
		attempted:   make(chan struct{}, 2),
		initialize:  initialize,
		initialized: make(chan error, 1),
		unlocked:    make(chan federationCredentialsUnlock, 2),
	}
}

func (l *controlledFederationCredentialsLock) Lock() {
	ready := make(chan struct{})
	l.state.Lock()
	first := l.attempts == 0
	l.attempts++
	if l.held {
		l.waiters = append(l.waiters, ready)
	} else {
		l.held = true
		close(ready)
	}
	l.state.Unlock()

	l.attempted <- struct{}{}
	if first {
		l.initialized <- l.initialize()
	}
	<-ready
}

func (l *controlledFederationCredentialsLock) Unlock() {
	credentials, err := readFederationCredentials()

	l.state.Lock()
	l.unlocked <- federationCredentialsUnlock{credentials: credentials, err: err}
	if len(l.waiters) == 0 {
		l.held = false
	} else {
		next := l.waiters[0]
		l.waiters = l.waiters[1:]
		close(next)
	}
	l.state.Unlock()
}

var _ sync.Locker = (*controlledFederationCredentialsLock)(nil)

func TestUpdateFederationCredentialsSerializesWholeMutation(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	lock := newControlledFederationCredentialsLock(func() error {
		return writeFederationCredentials(&FederationCredentials{
			Projects: map[string]FederationCredential{
				"project-seed": {Token: "token-seed"},
			},
		})
	})

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	}()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- updateFederationCredentialsWithLock(lock, func(credentials *FederationCredentials) error {
			close(firstEntered)
			<-releaseFirst
			credentials.Projects["project-a"] = FederationCredential{Token: "token-a"}
			return nil
		})
	}()
	<-lock.attempted
	require.NoError(t, <-lock.initialized)
	<-firstEntered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- updateFederationCredentialsWithLock(lock, func(credentials *FederationCredentials) error {
			close(secondEntered)
			credentials.Projects["project-b"] = FederationCredential{Token: "token-b"}
			return nil
		})
	}()
	<-lock.attempted
	select {
	case <-lock.unlocked:
		t.Fatal("first mutation unlocked while its callback was blocked")
	default:
	}
	select {
	case <-secondEntered:
		t.Fatal("second update callback entered before the first mutation unlocked")
	default:
	}

	close(releaseFirst)
	firstUnlock := <-lock.unlocked
	require.NoError(t, firstUnlock.err)
	assert.Contains(t, firstUnlock.credentials.Projects, "project-seed", "first mutation must read after locking")
	assert.Contains(t, firstUnlock.credentials.Projects, "project-a", "first mutation must write before unlocking")
	require.NoError(t, <-firstDone)
	<-secondEntered
	secondUnlock := <-lock.unlocked
	require.NoError(t, secondUnlock.err)
	assert.Contains(t, secondUnlock.credentials.Projects, "project-seed")
	assert.Contains(t, secondUnlock.credentials.Projects, "project-a")
	assert.Contains(t, secondUnlock.credentials.Projects, "project-b")
	require.NoError(t, <-secondDone)

	credentials, err := ReadFederationCredentials()
	require.NoError(t, err)
	assert.Contains(t, credentials.Projects, "project-seed")
	assert.Contains(t, credentials.Projects, "project-a")
	assert.Contains(t, credentials.Projects, "project-b")
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

func TestReserveFederationCredentialPartialWriteLeavesOldFileUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	const (
		localUID = "01HZNQ7VFPK1XGD8R5MABCD4EW"
		hubUID   = "01HZNQ7VFPK1XGD8R5MABCD4EX"
		otherUID = "01HZNQ7VFPK1XGD8R5MABCD4EY"
	)
	other := FederationCredential{
		HubURL: "https://other.example", HubProjectID: 7,
		Token: "token-b", Capabilities: "pull", Actor: "user-b",
	}
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
		return errors.New("injected reservation partial write failure")
	}
	t.Cleanup(func() { writeFederationCredentialsTempFile = originalWriter })

	reservation := FederationCredential{
		HubURL: "https://hub.example", HubProjectID: 42,
		Token: "token-a", Capabilities: "claim,pull,push",
		ManagedByConfig: true, HubCatalog: "primary",
		HubProjectName: "hub-project", RequestedActor: "user-a",
	}
	err = ReserveFederationCredentials(FederationCredentialReservation{
		ProjectUIDs: []string{localUID, hubUID},
		Credential:  reservation,
	})
	require.ErrorContains(t, err, "injected reservation partial write failure")

	after, err := os.ReadFile(path) //nolint:gosec // path is the test's isolated KATA_HOME.
	require.NoError(t, err)
	assert.Equal(t, before, after)
	credentials, err := ReadFederationCredentials()
	require.NoError(t, err)
	assert.NotContains(t, credentials.Projects, localUID)
	assert.NotContains(t, credentials.Projects, hubUID)
	assert.Equal(t, other, credentials.Projects[otherUID])
	assertNoFederationCredentialTempFiles(t, home)
}

func assertNoFederationCredentialTempFiles(t *testing.T, home string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, ".credentials.toml.tmp-*"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}
