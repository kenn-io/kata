package daemon_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type bridgeCandidateChangedAtBindingStore struct {
	db.Storage
	credentials *replicaCredentialStore
	projectUID  string
	activation  bool
}

func (s bridgeCandidateChangedAtBindingStore) UpsertFederationBinding(ctx context.Context, binding db.FederationBinding) (db.FederationBinding, error) {
	result, err := s.Storage.UpsertFederationBinding(ctx, binding)
	if err != nil || s.activation {
		return result, err
	}
	candidate, found, err := s.credentials.FederationCredential(ctx, s.projectUID)
	if err != nil || !found {
		return result, err
	}
	candidate.Token = "replacement-before-drain-test-token"
	return result, s.credentials.StoreFederationCredential(ctx, s.projectUID, candidate)
}

func (s bridgeCandidateChangedAtBindingStore) SetRelayBindingConfig(ctx context.Context, id int64, config db.RelayBindingConfig, expected ...db.RelayBindingConfig) (db.FederationBinding, error) {
	result, err := s.Storage.SetRelayBindingConfig(ctx, id, config, expected...)
	if err != nil || !s.activation {
		return result, err
	}
	candidate, found, err := s.credentials.FederationCredential(ctx, s.projectUID)
	if err != nil || !found {
		return result, err
	}
	candidate.Token = "replacement-before-drain-test-token"
	return result, s.credentials.StoreFederationCredential(ctx, s.projectUID, candidate)
}

// R3/R4: activation remains fenced by the pre-network candidate throughout
// local setup, including a replacement before its transport-drain snapshot.
func TestEnsureFederationReplicaCandidateChangedBeforeDrain(t *testing.T) {
	for _, boundary := range []string{"before_drain", "activation"} {
		t.Run(boundary, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				credentials := newReplicaCredentialStore()
				params := relayReplicaParams(t, store)
				params.Credential.Capabilities = "pull,push"
				expected := params.Credential
				expected.RelayEnrollmentPending = true
				params.ExpectedCredential = &expected
				require.NoError(t, credentials.StoreFederationCredential(t.Context(), params.HubProjectUID, expected))
				wrapped := bridgeCandidateChangedAtBindingStore{Storage: store, credentials: credentials, projectUID: params.HubProjectUID, activation: boundary == "activation"}
				_, err := daemon.EnsureFederationReplica(t.Context(), wrapped, credentials, nil, params)
				require.ErrorIs(t, err, daemon.ErrFederationReplicaReservationChanged)
				retained, found, err := credentials.FederationCredential(t.Context(), params.HubProjectUID)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, "replacement-before-drain-test-token", retained.Token)
				require.True(t, retained.RelayEnrollmentPending)
				project, err := store.ProjectByUID(t.Context(), params.HubProjectUID)
				require.NoError(t, err)
				binding, err := store.FederationBindingByProject(t.Context(), project.ID)
				require.NoError(t, err)
				if boundary == "before_drain" {
					require.Nil(t, binding.RelayConfig)
				}
				_, err = store.RootAuthority(t.Context(), project.UID)
				if boundary == "before_drain" {
					require.ErrorIs(t, err, db.ErrNotFound)
				}
			})
		})
	}
}
