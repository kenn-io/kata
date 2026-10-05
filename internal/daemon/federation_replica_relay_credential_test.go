package daemon_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

// R3/R4 and Review16807: a credential changed while sync drains must not be
// overwritten by an earlier enrollment. All waits are released and joined.
func TestEnsureFederationReplicaRelayRechecksCredentialAfterDrain(t *testing.T) {
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
		done := make(chan error, 1)
		go func() {
			_, err := daemon.EnsureFederationReplica(t.Context(), started, credentials, nil, params)
			done <- err
		}()
		select {
		case <-started.bindingWritten:
		case <-time.After(5 * time.Second):
			t.Fatal("relay setup did not reach drain")
		}
		// This completes only after the draining setup releases the global mutex;
		// its credential snapshot is therefore captured before replacement below.
		other := replicaServiceParams()
		other.HubProjectUID = "00000000000000000000000009"
		other.ProjectName = "unrelated-replica"
		other.HubProjectID = 43
		other.Credential.HubProjectID = 43
		_, err = daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, other)
		require.NoError(t, err)
		current, found, err := credentials.FederationCredential(t.Context(), base.Project.UID)
		require.NoError(t, err)
		require.True(t, found)
		current.Token = "rotated-upstream-test-token"
		require.NoError(t, credentials.StoreFederationCredential(t.Context(), base.Project.UID, current))
		release()
		select {
		case err := <-done:
			require.ErrorIs(t, err, daemon.ErrFederationReplicaReservationChanged)
		case <-time.After(5 * time.Second):
			t.Fatal("relay setup did not finish after drain")
		}
		retained, found, err := credentials.FederationCredential(t.Context(), base.Project.UID)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, current, retained)
		binding, err := store.FederationBindingByProject(t.Context(), base.Project.ID)
		require.NoError(t, err)
		require.Nil(t, binding.RelayConfig)
		_, err = store.RootAuthority(t.Context(), base.Project.UID)
		require.ErrorIs(t, err, db.ErrNotFound)
	})
}
