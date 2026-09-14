package config_test

import (
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func providerReservation() config.FederationManagedCredentialReservation {
	return config.FederationManagedCredentialReservation{
		ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW",
		Credential: config.FederationCredential{
			HubURL: "https://hub.example/tools/tasks", Token: "synthetic-saved-token",
			ManagedByConfig: true, HubCatalog: "example-hub",
			HubProjectName: "hub-project", SpokeProjectName: "spoke-project",
			Provider: &config.FederationProviderCredential{
				RequestID: uuid.MustParse("8b60f249-b495-4f17-8999-c64382e05680"),
				Command:   []string{"example-credential-provider", "--profile", "work profile"},
				Intent:    "collaborate", SpokeInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
				LocalProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW",
				Status:          "approval_required",
			},
		},
	}
}

func TestProviderLookupDoesNotFollowAReusedProjectName(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	store := config.DefaultFederationCredentialStore()
	saved := providerReservation()
	require.NoError(t, store.ReserveManagedFederationCredential(t.Context(), saved))

	// The original project's UID stays the same after a rename. A different
	// project may then take its old name, but not its provider connection.
	match, found, err := config.FindProjectManagedCredential(t.Context(), store, saved.ProjectUID, "renamed-project")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, saved.ProjectUID, match.ProjectUID)
	_, found, err = config.FindProjectManagedCredential(t.Context(), store, "01ARZ3NDEKTSV4RRFFQ69G5FAX", "spoke-project")
	require.NoError(t, err)
	assert.False(t, found)

	// Catalog enrollment still needs its name lookup before a project rekey.
	saved.Credential.Provider = nil
	require.NoError(t, store.StoreFederationCredential(t.Context(), saved.ProjectUID, saved.Credential))
	match, found, err = config.FindProjectManagedCredential(t.Context(), store, "01ARZ3NDEKTSV4RRFFQ69G5FAX", "spoke-project")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, saved.ProjectUID, match.ProjectUID)
}

func TestProviderReservationResumesAcrossCredentialReadsAndRekey(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	initial := providerReservation()
	require.NoError(t, config.ReserveManagedFederationCredential(initial))

	// File reads reconstruct provider values at different addresses. Exact
	// replay must still recognize the same saved request and token.
	store := config.DefaultFederationCredentialStore()
	saved, found, err := store.FindManagedFederationCredential(ctx, "spoke-project")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, saved.Credential.Provider)
	assert.Equal(t, initial.Credential.Provider.RequestID, saved.Credential.Provider.RequestID)
	assert.Equal(t, "synthetic-saved-token", saved.Credential.Token)
	assert.Equal(t, []string{"example-credential-provider", "--profile", "work profile"}, saved.Credential.Provider.Command)
	require.NoError(t, store.ReserveManagedFederationCredential(ctx, saved))

	ready := providerReservation()
	ready.Credential.HubProjectID = 7
	ready.Credential.Actor = "Example Operator"
	ready.Credential.Capabilities = "claim,pull,push"
	ready.Credential.Provider.Status = "ready"
	ready.Credential.Provider.HubProjectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	ready.Credential.Provider.EnrollmentID = 9
	ready.Credential.Provider.ExpiresAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, store.ReplaceManagedFederationCredential(ctx, saved, ready))
	rekey := config.FederationCredentialRekey{
		FromProjectUID: initial.ProjectUID, ToProjectUID: ready.Credential.Provider.HubProjectUID,
		Expected: ready.Credential, Replacement: ready.Credential,
	}
	require.NoError(t, store.RekeyFederationCredential(ctx, rekey))
	require.NoError(t, store.RekeyFederationCredential(ctx, rekey), "exact rekey retry must not lose the operation")

	reopened := config.DefaultFederationCredentialStore()
	resumed, found, err := reopened.FindManagedFederationCredential(ctx, "spoke-project")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAX", resumed.ProjectUID)
	assert.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAW", resumed.Credential.Provider.LocalProjectUID)
	assert.Equal(t, int64(9), resumed.Credential.Provider.EnrollmentID)
	assert.Equal(t, "2030-01-01T00:00:00Z", resumed.Credential.Provider.ExpiresAt.Format(time.RFC3339))
	assert.Equal(t, "synthetic-saved-token", resumed.Credential.Token)

	// Cleanup by the old key cannot erase the moved operation.
	require.ErrorIs(t, reopened.DeleteManagedFederationCredential(ctx, ready), config.ErrFederationCredentialConflict)
	resumed.Credential.LeavePending = true
	expected := resumed
	expected.Credential.LeavePending = false
	require.NoError(t, reopened.ReplaceManagedFederationCredential(ctx, expected, resumed))
	require.NoError(t, reopened.DeleteManagedFederationCredential(ctx, resumed))
	_, found, err = reopened.FindManagedFederationCredential(ctx, "spoke-project")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestProviderReservationRejectsChangedExpectedOperation(t *testing.T) {
	changes := map[string]func(*config.FederationProviderCredential){
		"request":        func(p *config.FederationProviderCredential) { p.RequestID = uuid.New() },
		"command":        func(p *config.FederationProviderCredential) { p.Command[2] = "other profile" },
		"intent":         func(p *config.FederationProviderCredential) { p.Intent = "migrate" },
		"installation":   func(p *config.FederationProviderCredential) { p.SpokeInstanceUID = "01ARZ3NDEKTSV4RRFFQ69G5FAX" },
		"local project":  func(p *config.FederationProviderCredential) { p.LocalProjectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAY" },
		"remote project": func(p *config.FederationProviderCredential) { p.HubProjectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAZ" },
		"enrollment":     func(p *config.FederationProviderCredential) { p.EnrollmentID = 99 },
		"expiry": func(p *config.FederationProviderCredential) {
			p.ExpiresAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		},
		"status": func(p *config.FederationProviderCredential) { p.Status = "denied" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			original := providerReservation()
			require.NoError(t, config.ReserveManagedFederationCredential(original))
			stale := providerReservation()
			change(stale.Credential.Provider)
			replacement := providerReservation()
			replacement.Credential.LeavePending = true
			require.ErrorIs(t, config.ReplaceManagedFederationCredential(stale, replacement), config.ErrFederationCredentialConflict)
			require.ErrorIs(t, config.DeleteManagedFederationCredential(stale), config.ErrFederationCredentialConflict)
			saved, found, err := config.FindManagedFederationCredential("spoke-project")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, original, saved, "rejected updates must preserve the original request")
		})
	}
}
