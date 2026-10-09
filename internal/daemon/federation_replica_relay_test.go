package daemon_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

func relayReplicaParams(t *testing.T, store db.Storage) daemon.EnsureFederationReplicaParams {
	t.Helper()
	p := replicaServiceParams()
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	root := db.RootKeyPin{ProjectUID: p.HubProjectUID, AuthorityUID: "00000000000000000000000002", PublicKey: public, KeyID: db.RootPublicKeyID(public)}
	p.Relay = &api.RelayHandshake{ProtocolVersion: 1, BindingUID: "00000000000000000000000008", UpstreamInstanceUID: root.AuthorityUID, ResetEpoch: 1, HubPath: []string{root.AuthorityUID, store.InstanceUID()}, Root: root}
	p.RelayLocalActor = "local-member"
	p.RelayServeDownstream = true
	return p
}

type relayReplicaStartedStore struct {
	db.Storage
	started chan struct{}
	once    sync.Once
}

type relayReplicaBindingStartedStore struct {
	*relayReplicaStartedStore
	bindingWritten chan struct{}
	bindingOnce    sync.Once
}

func (s *relayReplicaBindingStartedStore) UpsertFederationBinding(ctx context.Context, binding db.FederationBinding) (db.FederationBinding, error) {
	result, err := s.Storage.UpsertFederationBinding(ctx, binding)
	s.bindingOnce.Do(func() { close(s.bindingWritten) })
	return result, err
}

// Review16807: waiting for one project's transport must not hold the global
// replica mutex and block unrelated project enrollment.
func TestEnsureFederationReplicaRelayDrainAllowsOtherProjects(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		credentials := newReplicaCredentialStore()
		base, err := daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, replicaServiceParams())
		require.NoError(t, err)
		finish, err := federationcoord.BeginSync(t.Context(), federationcoord.Key(store.InstanceUID(), base.Project.ID), store, base.Project.ID)
		require.NoError(t, err)
		var once sync.Once
		release := func() { once.Do(finish) }
		defer release()
		started := &relayReplicaBindingStartedStore{relayReplicaStartedStore: &relayReplicaStartedStore{Storage: store, started: make(chan struct{})}, bindingWritten: make(chan struct{})}
		params := relayReplicaParams(t, store)
		draining := make(chan error, 1)
		go func() {
			_, err := daemon.EnsureFederationReplica(t.Context(), started, credentials, nil, params)
			draining <- err
		}()
		select {
		case <-started.bindingWritten:
		case <-time.After(5 * time.Second):
			t.Fatal("relay setup did not reach the drain")
		}
		other := replicaServiceParams()
		other.HubProjectUID = "00000000000000000000000009"
		other.ProjectName = "unrelated-replica"
		other.HubProjectID = 43
		other.Credential.HubProjectID = 43
		done := make(chan error, 1)
		go func() {
			_, err := daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, other)
			done <- err
		}()
		completedBeforeRelease := false
		select {
		case err := <-done:
			require.NoError(t, err)
			completedBeforeRelease = true
		case <-time.After(2 * time.Second):
		}
		release()
		select {
		case err := <-draining:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("relay setup did not finish after release")
		}
		if !completedBeforeRelease {
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("unrelated enrollment did not finish after release")
			}
		}
		require.True(t, completedBeforeRelease, "one project's sync drain held the global replica mutex")
	})
}

func (s *relayReplicaStartedStore) RootAuthority(ctx context.Context, projectUID string) (db.RootKeyPin, error) {
	s.once.Do(func() { close(s.started) })
	return s.Storage.RootAuthority(ctx, projectUID)
}

func (s *relayReplicaStartedStore) AcquireFederationProjectExclusiveLock(ctx context.Context, projectID int64) (func(), error) {
	if locker, ok := s.Storage.(interface {
		AcquireFederationProjectExclusiveLock(context.Context, int64) (func(), error)
	}); ok {
		return locker.AcquireFederationProjectExclusiveLock(ctx, projectID)
	}
	return func() {}, nil
}

// R4/R9: changing an existing replica to relay mode drains its current sync
// before changing the trusted root/hop or activating a new credential.
func TestEnsureFederationReplicaRelayDrainsExistingSync(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		credentials := newReplicaCredentialStore()
		base, err := daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, replicaServiceParams())
		require.NoError(t, err)
		finish, err := federationcoord.BeginSync(t.Context(), federationcoord.Key(store.InstanceUID(), base.Project.ID), store, base.Project.ID)
		require.NoError(t, err)
		var once sync.Once
		release := func() { once.Do(finish) }
		defer release()
		started := &relayReplicaStartedStore{Storage: store, started: make(chan struct{})}
		params := relayReplicaParams(t, store)
		done := make(chan error, 1)
		go func() {
			_, err := daemon.EnsureFederationReplica(t.Context(), started, credentials, nil, params)
			done <- err
		}()
		select {
		case <-started.started:
		case <-time.After(5 * time.Second):
			t.Fatal("relay setup did not begin")
		}
		select {
		case err := <-done:
			require.FailNow(t, "relay setup completed while an existing sync was active", "%v", err)
		case <-time.After(time.Second):
		}
		binding, err := store.FederationBindingByProject(t.Context(), base.Project.ID)
		require.NoError(t, err)
		require.Nil(t, binding.RelayConfig)
		_, err = store.RootAuthority(t.Context(), base.Project.UID)
		require.ErrorIs(t, err, db.ErrNotFound)
		release()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("relay setup did not finish after sync drained")
		}
	})
}

// R1/R3/R4/R9: installing a negotiated replica must retain the root and hop
// before its credential becomes usable, including retry after credential I/O.
func TestEnsureFederationReplicaNegotiatedRelay(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		params := relayReplicaParams(t, store)
		credentials := newReplicaCredentialStore()
		credentials.storeErr = errors.New("credential storage unavailable")
		result, err := daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
		require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialIO)
		binding, err := store.FederationBindingByProject(t.Context(), result.Project.ID)
		require.NoError(t, err)
		require.NotNil(t, binding.RelayConfig, "relay state must precede credential persistence")
		require.Equal(t, params.Relay.BindingUID, binding.RelayConfig.BindingUID)
		require.Equal(t, params.RelayLocalActor, binding.RelayConfig.LocalActor)
		require.Equal(t, params.Credential.Actor, binding.Actor, "local setup must not replace the credential-bound upstream account")
		require.True(t, binding.RelayConfig.ServeDownstream)
		pin, err := store.RootAuthority(t.Context(), params.HubProjectUID)
		require.NoError(t, err)
		require.Equal(t, params.Relay.Root, pin)
		credentials.storeErr = nil
		wakes := 0
		retry, err := daemon.EnsureFederationReplica(t.Context(), store, credentials, func() { wakes++ }, params)
		require.NoError(t, err)
		require.Equal(t, result.Project.ID, retry.Project.ID)
		require.NotNil(t, retry.Binding.RelayConfig)
		require.True(t, retry.Binding.PushEnabled)
		require.Equal(t, 1, wakes)
	})
}

// R10: unsupported protocol, mismatched project/root/path, or one-way grants
// fail before creating a project, root pin, binding, or saved credential.
func TestEnsureFederationReplicaRejectsMalformedRelayBeforeMutation(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, name := range []string{"protocol", "project", "path", "key", "pull_only", "missing_pull"} {
			t.Run(name, func(t *testing.T) {
				params := relayReplicaParams(t, store)
				switch name {
				case "protocol":
					params.Relay.ProtocolVersion = 2
				case "project":
					params.Relay.Root.ProjectUID = replicaLocalProjectUID
				case "path":
					params.Relay.HubPath = []string{params.Relay.Root.AuthorityUID, params.Relay.Root.AuthorityUID, store.InstanceUID()}
				case "key":
					params.Relay.Root.PublicKey = []byte{1}
				case "pull_only":
					params.PushEnabled = false
				case "missing_pull":
					params.Credential.Capabilities = "push"
				}
				credentials := newReplicaCredentialStore()
				_, err := daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
				require.Error(t, err)
				_, err = store.ProjectByUID(t.Context(), params.HubProjectUID)
				require.ErrorIs(t, err, db.ErrNotFound)
				_, err = store.RootAuthority(t.Context(), params.HubProjectUID)
				require.ErrorIs(t, err, db.ErrNotFound)
				require.Zero(t, credentials.storeCalls)
			})
		}
	})
}
