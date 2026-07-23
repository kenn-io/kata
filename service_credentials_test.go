package kata

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

const (
	serviceAdapterSourceProjectUID = "01HZNQ7VFPK1XGD8R5MABCD4EZ"
	serviceAdapterTargetProjectUID = "01HZNQ7VFPK1XGD8R5MABCD4F0"
)

func TestServiceCredentialStoreAdapterReserveManagedIsBoundedAndNonMutating(t *testing.T) {
	ctx := t.Context()
	store := newServiceFederationCredentialStore()
	original := serviceAdapterPublicCredential()
	require.NoError(t, store.StoreFederationCredential(
		ctx, serviceAdapterTargetProjectUID, original,
	))
	adapter := serviceCredentialStoreAdapter{store: store}

	err := adapter.ReserveManagedFederationCredential(
		ctx,
		config.FederationManagedCredentialReservation{
			ProjectUID: serviceAdapterTargetProjectUID,
			Credential: serviceAdapterManagedCredential(),
		},
	)

	assertBoundedManagedUnsupportedError(t, err)
	assertPublicCredentialUnchanged(
		ctx, t, store, serviceAdapterTargetProjectUID, original,
	)
}

func TestServiceCredentialStoreAdapterRekeyIsBoundedAndNonMutating(t *testing.T) {
	ctx := t.Context()
	store := newServiceFederationCredentialStore()
	source := serviceAdapterPublicCredential()
	target := source
	target.Token = "target-token"
	require.NoError(t, store.StoreFederationCredential(
		ctx, serviceAdapterSourceProjectUID, source,
	))
	require.NoError(t, store.StoreFederationCredential(
		ctx, serviceAdapterTargetProjectUID, target,
	))
	adapter := serviceCredentialStoreAdapter{store: store}

	err := adapter.RekeyFederationCredential(
		ctx,
		config.FederationCredentialRekey{
			FromProjectUID: serviceAdapterSourceProjectUID,
			ToProjectUID:   serviceAdapterTargetProjectUID,
			Expected:       internalFederationCredential(source),
			Replacement:    serviceAdapterManagedCredential(),
		},
	)

	assertBoundedManagedUnsupportedError(t, err)
	assertPublicCredentialUnchanged(
		ctx, t, store, serviceAdapterSourceProjectUID, source,
	)
	assertPublicCredentialUnchanged(
		ctx, t, store, serviceAdapterTargetProjectUID, target,
	)
}

func TestServiceCredentialStoreAdapterDeleteManagedIsBoundedAndNonMutating(t *testing.T) {
	ctx := t.Context()
	store := newServiceFederationCredentialStore()
	original := serviceAdapterPublicCredential()
	require.NoError(t, store.StoreFederationCredential(
		ctx, serviceAdapterTargetProjectUID, original,
	))
	adapter := serviceCredentialStoreAdapter{store: store}

	err := adapter.DeleteManagedFederationCredential(
		ctx,
		config.FederationManagedCredentialReservation{
			ProjectUID: serviceAdapterTargetProjectUID,
			Credential: serviceAdapterManagedCredential(),
		},
	)

	assertBoundedManagedUnsupportedError(t, err)
	assertPublicCredentialUnchanged(
		ctx, t, store, serviceAdapterTargetProjectUID, original,
	)
}

func TestServiceCredentialStoreAdapterFindManagedReturnsCleanNotFound(t *testing.T) {
	ctx := t.Context()
	store := newServiceFederationCredentialStore()
	original := serviceAdapterPublicCredential()
	require.NoError(t, store.StoreFederationCredential(
		ctx, serviceAdapterTargetProjectUID, original,
	))
	adapter := serviceCredentialStoreAdapter{store: store}

	reservation, found, err := adapter.FindManagedFederationCredential(
		ctx, "spoke-project",
	)

	require.NoError(t, err)
	assert.False(t, found)
	assert.Zero(t, reservation)
	assertPublicCredentialUnchanged(
		ctx, t, store, serviceAdapterTargetProjectUID, original,
	)
}

func assertBoundedManagedUnsupportedError(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, errManagedFederationCredentialsUnsupported)
	assert.EqualError(
		t, err,
		"mounted federation credential store does not support managed operations",
	)
}

func assertPublicCredentialUnchanged(
	ctx context.Context,
	t *testing.T,
	store FederationCredentialStore,
	projectUID string,
	expected FederationCredential,
) {
	t.Helper()
	actual, found, err := store.FederationCredential(ctx, projectUID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, expected, actual)
}

func serviceAdapterPublicCredential() FederationCredential {
	return FederationCredential{
		HubURL:        "https://hub.example",
		HubProjectID:  42,
		Token:         "public-token",
		Capabilities:  "pull,push",
		Actor:         "example-actor",
		AllowInsecure: false,
	}
}

func serviceAdapterManagedCredential() config.FederationCredential {
	credential := internalFederationCredential(serviceAdapterPublicCredential())
	credential.Token = "managed-token"
	credential.ManagedByConfig = true
	credential.SpokeProjectName = "spoke-project"
	return credential
}
