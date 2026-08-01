package config_test

import (
	"context"
	"fmt"
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

func TestFederationTransportCredentialUsesBindingTarget(t *testing.T) {
	credential := config.FederationCredential{
		HubURL: "http://old-hub.example", HubProjectID: 41,
		Token: "enrollment-token", Capabilities: "pull,claim", Actor: "sync-agent",
		AllowInsecure: true,
	}

	got := config.FederationTransportCredential(
		"https://new-hub.example/reverse-proxy", 42, false, credential,
	)

	assert.Equal(t, "https://new-hub.example/reverse-proxy", got.HubURL)
	assert.Equal(t, int64(42), got.HubProjectID)
	assert.False(t, got.AllowInsecure)
	assert.Equal(t, "enrollment-token", got.Token)
	assert.Equal(t, "pull,claim", got.Capabilities)
	assert.Equal(t, "sync-agent", got.Actor)
}

func TestFederationTransportCredentialPreservesMatchingLegacyPlaintextPolicy(t *testing.T) {
	credential := config.FederationCredential{
		HubURL: "http://HUB.EXAMPLE:80/reverse-proxy/", HubProjectID: 42,
		Token: "enrollment-token", AllowInsecure: true,
	}

	got := config.FederationTransportCredential(
		"http://hub.example/reverse-proxy", 42, false, credential,
	)

	assert.True(t, got.AllowInsecure)
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

func TestFederationCredentialManagedMetadataBackwardCompatible(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	const (
		projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
		token      = "old-secret-token"
	)
	path := filepath.Join(home, "credentials.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[projects."01HZNQ7VFPK1XGD8R5MABCD4EX"]
hub_url = "http://127.0.0.1:7373"
hub_project_id = 42
token = "old-secret-token"
`), 0o600))

	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	got := creds.Projects[projectUID]
	assert.False(t, got.ManagedByConfig)
	assert.Empty(t, got.HubCatalog)
	assert.Empty(t, got.HubProjectName)
	assert.Empty(t, got.RequestedActor)
	assert.Empty(t, got.SpokeProjectName)

	require.NoError(t, config.WriteFederationCredential(projectUID, got))
	roundTripped, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, got, roundTripped.Projects[projectUID])

	data, err := os.ReadFile(path) //nolint:gosec // Test path is rooted in t.TempDir.
	require.NoError(t, err)
	assert.Contains(t, string(data), token)
	assert.NotContains(t, string(data), "managed_by_config")
	assert.NotContains(t, string(data), "spoke_project_name")
}

func TestFederationCredentialManagedMetadataRoundTripsAndRedactsToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	const (
		projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
		token      = "managed-secret-token"
	)

	require.NoError(t, config.WriteFederationCredential(projectUID,
		config.FederationCredential{
			HubURL:           "https://hub.example",
			HubProjectID:     42,
			Token:            token,
			ManagedByConfig:  true,
			HubCatalog:       "primary",
			HubProjectName:   "spoke-project",
			RequestedActor:   "automation-user",
			SpokeProjectName: "example-project",
		}))

	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	got := creds.Projects[projectUID]
	assert.True(t, got.ManagedByConfig)
	assert.Equal(t, "primary", got.HubCatalog)
	assert.Equal(t, "spoke-project", got.HubProjectName)
	assert.Equal(t, "automation-user", got.RequestedActor)
	assert.Equal(t, "example-project", got.SpokeProjectName)
	assert.Equal(t, token, got.Token)

	path := filepath.Join(home, "credentials.toml")
	data, err := os.ReadFile(path) //nolint:gosec // Test path is rooted in t.TempDir.
	require.NoError(t, err)
	assert.Contains(t, string(data), token)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	metadata := config.FederationCredentialMetadataFor(projectUID)
	assert.True(t, metadata.ManagedByConfig)
	assert.Equal(t, "primary", metadata.HubCatalog)
	assert.Equal(t, "spoke-project", metadata.HubProjectName)
	assert.Equal(t, "automation-user", metadata.RequestedActor)
	assert.Equal(t, "example-project", metadata.SpokeProjectName)
	assert.NotContains(t, fmt.Sprintf("%+v", metadata), token)
}

func TestReserveFederationCredentialUsesOneExactHubUID(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	store := config.DefaultFederationCredentialStore()
	managed := config.FederationCredential{
		HubURL:           "https://daemon.example",
		HubProjectID:     42,
		Token:            "pending-token",
		ManagedByConfig:  true,
		SpokeProjectName: "spoke-project",
	}

	require.NoError(t, store.ReserveManagedFederationCredential(ctx,
		config.FederationManagedCredentialReservation{
			ProjectUID: "01HUBPROJECT00000000000000",
			Credential: managed,
		}))
	require.NoError(t, store.ReserveManagedFederationCredential(ctx,
		config.FederationManagedCredentialReservation{
			ProjectUID: "01HUBPROJECT00000000000000",
			Credential: managed,
		}))

	match, found, err := store.FindManagedFederationCredential(ctx, "spoke-project")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "01HUBPROJECT00000000000000", match.ProjectUID)
	assert.Equal(t, managed, match.Credential)
}

func TestConcurrentManagedCredentialReservationsPreserveEveryEntry(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	store := config.DefaultFederationCredentialStore()
	const (
		repetitions = 12
		workers     = 12
	)
	expected := make(map[string]config.FederationCredential, repetitions*workers)

	for repetition := range repetitions {
		start := make(chan struct{})
		results := make(chan error, workers)
		for worker := range workers {
			projectUID := fmt.Sprintf("managed-%02d-%02d", repetition, worker)
			credential := config.FederationCredential{
				HubURL:           fmt.Sprintf("https://hub-%02d.example", repetition),
				HubProjectID:     int64(repetition*workers + worker + 1),
				Token:            fmt.Sprintf("token-%02d-%02d", repetition, worker),
				ManagedByConfig:  true,
				SpokeProjectName: fmt.Sprintf("spoke-project-%02d-%02d", repetition, worker),
			}
			expected[projectUID] = credential
			go func() {
				<-start
				results <- store.ReserveManagedFederationCredential(
					ctx,
					config.FederationManagedCredentialReservation{
						ProjectUID: projectUID,
						Credential: credential,
					},
				)
			}()
		}
		close(start)
		for range workers {
			require.NoError(t, <-results)
		}

		credentials, err := config.ReadFederationCredentials()
		require.NoError(t, err)
		require.Len(t, credentials.Projects, len(expected))
		for projectUID, credential := range expected {
			assert.Equal(t, credential, credentials.Projects[projectUID], projectUID)
		}
	}
}

func TestReserveFederationCredentialConflictDoesNotRewriteFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	ctx := context.Background()
	store := config.DefaultFederationCredentialStore()
	const hubUID = "01HUBPROJECT00000000000000"
	require.NoError(t, store.StoreFederationCredential(ctx, hubUID,
		config.FederationCredential{
			HubURL:       "https://daemon.example",
			HubProjectID: 42,
			Token:        "manual-token",
		}))
	path := filepath.Join(home, "credentials.toml")
	before, err := os.ReadFile(path) //nolint:gosec // Test path is rooted in t.TempDir.
	require.NoError(t, err)

	err = store.ReserveManagedFederationCredential(ctx,
		config.FederationManagedCredentialReservation{
			ProjectUID: hubUID,
			Credential: config.FederationCredential{
				HubURL:           "https://daemon.example",
				HubProjectID:     42,
				Token:            "pending-token",
				ManagedByConfig:  true,
				SpokeProjectName: "spoke-project",
			},
		})
	require.ErrorIs(t, err, config.ErrFederationCredentialConflict)
	after, err := os.ReadFile(path) //nolint:gosec // Test path is rooted in t.TempDir.
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestFindManagedFederationCredentialRejectsTwoMarkedEntries(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	store := config.DefaultFederationCredentialStore()
	managed := config.FederationCredential{
		HubURL: "https://daemon.example", HubProjectID: 42, Token: "pending-token",
		ManagedByConfig: true, SpokeProjectName: "spoke-project",
	}
	require.NoError(t, store.StoreFederationCredential(ctx,
		"01HUBPROJECT00000000000000", managed))
	require.NoError(t, store.StoreFederationCredential(ctx,
		"01HUBPROJECT00000000000001", managed))

	_, _, err := store.FindManagedFederationCredential(ctx, "spoke-project")
	require.ErrorIs(t, err, config.ErrFederationCredentialConflict)
	credentials, readErr := config.ReadFederationCredentials()
	require.NoError(t, readErr)
	assert.Equal(t, managed, credentials.Projects["01HUBPROJECT00000000000000"])
	assert.Equal(t, managed, credentials.Projects["01HUBPROJECT00000000000001"])
}

func TestDeleteManagedFederationCredentialRequiresExactKeyAndValue(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	store := config.DefaultFederationCredentialStore()
	match := config.FederationManagedCredentialReservation{
		ProjectUID: "01HUBPROJECT00000000000000",
		Credential: config.FederationCredential{
			HubURL: "https://daemon.example", HubProjectID: 42, Token: "pending-token",
			ManagedByConfig: true, SpokeProjectName: "spoke-project",
		},
	}
	require.NoError(t, store.ReserveManagedFederationCredential(ctx, match))

	wrongKey := match
	wrongKey.ProjectUID = "01HUBPROJECT00000000000001"
	require.NoError(t, store.StoreFederationCredential(ctx, wrongKey.ProjectUID,
		config.FederationCredential{Token: "manual-token"}))
	require.ErrorIs(t, store.DeleteManagedFederationCredential(ctx, wrongKey),
		config.ErrFederationCredentialConflict)
	wrongValue := match
	wrongValue.Credential.Token = "changed-token"
	require.NoError(t, store.StoreFederationCredential(ctx, match.ProjectUID,
		wrongValue.Credential))
	require.ErrorIs(t, store.DeleteManagedFederationCredential(ctx, match),
		config.ErrFederationCredentialConflict)
	credentials, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, wrongValue.Credential, credentials.Projects[match.ProjectUID])
	assert.Equal(t, config.FederationCredential{Token: "manual-token"},
		credentials.Projects[wrongKey.ProjectUID])

	require.NoError(t, store.StoreFederationCredential(ctx, match.ProjectUID, match.Credential))
	require.NoError(t, store.DeleteManagedFederationCredential(ctx, match))
	credentials, err = config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.NotContains(t, credentials.Projects, match.ProjectUID)
}

func TestDeleteManagedFederationCredentialRejectsReservationMovedToAnotherKey(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	store := config.DefaultFederationCredentialStore()
	match := config.FederationManagedCredentialReservation{
		ProjectUID: "01HUBPROJECT00000000000000",
		Credential: config.FederationCredential{
			HubURL: "https://daemon.example", HubProjectID: 42, Token: "pending-token",
			ManagedByConfig: true, SpokeProjectName: "spoke-project",
		},
	}
	require.NoError(t, store.ReserveManagedFederationCredential(ctx, match))
	require.NoError(t, store.DeleteFederationCredential(ctx, match.ProjectUID))
	movedUID := "01HUBPROJECT00000000000001"
	require.NoError(t, store.StoreFederationCredential(ctx, movedUID, match.Credential))

	err := store.DeleteManagedFederationCredential(ctx, match)

	require.ErrorIs(t, err, config.ErrFederationCredentialConflict)
	credentials, readErr := config.ReadFederationCredentials()
	require.NoError(t, readErr)
	assert.Equal(t, match.Credential, credentials.Projects[movedUID])
}

func TestReplaceManagedFederationCredentialRequiresExactCurrentValue(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	store := config.DefaultFederationCredentialStore()
	current := config.FederationManagedCredentialReservation{
		ProjectUID: "01HUBPROJECT00000000000000",
		Credential: config.FederationCredential{
			HubURL: "https://daemon.example", HubProjectID: 42, Token: "pending-token",
			ManagedByConfig: true, SpokeProjectName: "spoke-project",
		},
	}
	require.NoError(t, store.ReserveManagedFederationCredential(ctx, current))
	replacement := current
	replacement.Credential.LeavePending = true
	require.NoError(t, store.ReplaceManagedFederationCredential(ctx, current, replacement))

	stale := current
	stale.Credential.Token = "stale-token"
	err := store.ReplaceManagedFederationCredential(ctx, stale, current)

	require.ErrorIs(t, err, config.ErrFederationCredentialConflict)
	stored, found, readErr := store.FederationCredential(ctx, current.ProjectUID)
	require.NoError(t, readErr)
	require.True(t, found)
	assert.Equal(t, replacement.Credential, stored)
}

func TestRekeyFederationCredentialMovesManualLocalUIDToHubUIDOnce(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	store := config.DefaultFederationCredentialStore()
	manual := config.FederationCredential{
		HubURL: "https://daemon.example", HubProjectID: 42, Token: "manual-token",
	}
	replacement := manual
	replacement.ManagedByConfig = true
	replacement.SpokeProjectName = "spoke-project"
	rekey := config.FederationCredentialRekey{
		FromProjectUID: "01LOCALPROJECT000000000000",
		ToProjectUID:   "01HUBPROJECT00000000000000",
		Expected:       manual,
		Replacement:    replacement,
	}
	require.NoError(t, store.StoreFederationCredential(ctx, rekey.FromProjectUID, manual))
	require.NoError(t, store.RekeyFederationCredential(ctx, rekey))
	require.NoError(t, store.RekeyFederationCredential(ctx, rekey))

	credentials, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.NotContains(t, credentials.Projects, rekey.FromProjectUID)
	assert.Equal(t, replacement, credentials.Projects[rekey.ToProjectUID])
	assert.Len(t, credentials.Projects, 1)
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

func TestRekeyFederationCredentialAtomicallyMovesOneEntryAndPreservesOthers(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	const (
		localUID = "01HZNQ7VFPK1XGD8R5MABCD4EA"
		hubUID   = "01HZNQ7VFPK1XGD8R5MABCD4EX"
		otherUID = "01HZNQ7VFPK1XGD8R5MABCD4EY"
	)
	manual := config.FederationCredential{
		HubURL: "https://hub.example", HubProjectID: 42,
		Token: "token-a", Capabilities: "claim,pull,push",
		Actor: "user-a",
	}
	other := config.FederationCredential{
		HubURL: "https://other.example", HubProjectID: 7,
		Token: "token-b", Capabilities: "pull",
		Actor: "user-b",
	}
	require.NoError(t, config.WriteFederationCredential(localUID, manual))
	require.NoError(t, config.WriteFederationCredential(otherUID, other))

	replacement := manual
	replacement.Actor = "identity-user"
	replacement.ManagedByConfig = true
	replacement.HubCatalog = "primary"
	replacement.HubProjectName = "hub-project"
	replacement.RequestedActor = "user-a"
	require.NoError(t, config.RekeyFederationCredential(config.FederationCredentialRekey{
		FromProjectUID: localUID,
		ToProjectUID:   hubUID,
		Expected:       manual,
		Replacement:    replacement,
	}))

	credentials, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.NotContains(t, credentials.Projects, localUID)
	assert.Equal(t, replacement, credentials.Projects[hubUID])
	assert.Equal(t, other, credentials.Projects[otherUID])
	assert.Len(t, credentials.Projects, 2)
}

func TestRekeyFederationCredentialRejectsConcurrentSourceOrTargetChange(t *testing.T) {
	const (
		localUID = "01HZNQ7VFPK1XGD8R5MABCD4EA"
		hubUID   = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	)
	manual := config.FederationCredential{
		HubURL: "https://hub.example", HubProjectID: 42,
		Token: "token-a", Capabilities: "claim,pull,push",
		Actor: "user-a",
	}
	replacement := manual
	replacement.Actor = "identity-user"
	replacement.ManagedByConfig = true

	t.Run("source changed", func(t *testing.T) {
		t.Setenv("KATA_HOME", t.TempDir())
		changed := manual
		changed.Token = "token-c"
		require.NoError(t, config.WriteFederationCredential(localUID, changed))

		err := config.RekeyFederationCredential(config.FederationCredentialRekey{
			FromProjectUID: localUID,
			ToProjectUID:   hubUID,
			Expected:       manual,
			Replacement:    replacement,
		})
		require.ErrorIs(t, err, config.ErrFederationCredentialConflict)
		credentials, readErr := config.ReadFederationCredentials()
		require.NoError(t, readErr)
		assert.Equal(t, changed, credentials.Projects[localUID])
		assert.NotContains(t, credentials.Projects, hubUID)
	})

	t.Run("target changed", func(t *testing.T) {
		t.Setenv("KATA_HOME", t.TempDir())
		conflicting := manual
		conflicting.Token = "token-d"
		require.NoError(t, config.WriteFederationCredential(localUID, manual))
		require.NoError(t, config.WriteFederationCredential(hubUID, conflicting))

		err := config.RekeyFederationCredential(config.FederationCredentialRekey{
			FromProjectUID: localUID,
			ToProjectUID:   hubUID,
			Expected:       manual,
			Replacement:    replacement,
		})
		require.ErrorIs(t, err, config.ErrFederationCredentialConflict)
		credentials, readErr := config.ReadFederationCredentials()
		require.NoError(t, readErr)
		assert.Equal(t, manual, credentials.Projects[localUID])
		assert.Equal(t, conflicting, credentials.Projects[hubUID])
	})
}

func TestRekeyFederationCredentialConvergesAfterMatchingTargetWrite(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	const (
		localUID = "01HZNQ7VFPK1XGD8R5MABCD4EA"
		hubUID   = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	)
	manual := config.FederationCredential{
		HubURL: "https://hub.example", HubProjectID: 42,
		Token: "token-a", Capabilities: "claim,pull,push",
		Actor: "user-a",
	}
	replacement := manual
	replacement.Actor = "identity-user"
	replacement.ManagedByConfig = true

	// A matching direct join may store the same credential under the hub UID
	// before reconciliation reaches its compare-and-rekey step.
	require.NoError(t, config.WriteFederationCredential(localUID, manual))
	require.NoError(t, config.WriteFederationCredential(hubUID, manual))
	require.NoError(t, config.RekeyFederationCredential(config.FederationCredentialRekey{
		FromProjectUID: localUID,
		ToProjectUID:   hubUID,
		Expected:       manual,
		Replacement:    replacement,
	}))

	credentials, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.NotContains(t, credentials.Projects, localUID)
	assert.Equal(t, replacement, credentials.Projects[hubUID])
	assert.Len(t, credentials.Projects, 1)
}

func TestRekeyFederationCredentialExactCompletedRetryIsIdempotent(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	const (
		localUID = "01HZNQ7VFPK1XGD8R5MABCD4EA"
		hubUID   = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	)
	manual := config.FederationCredential{
		HubURL: "https://hub.example", HubProjectID: 42,
		Token: "token-a", Capabilities: "claim,pull,push",
		Actor: "user-a",
	}
	replacement := manual
	replacement.Actor = "identity-user"
	replacement.ManagedByConfig = true
	require.NoError(t, config.WriteFederationCredential(hubUID, replacement))

	require.NoError(t, config.RekeyFederationCredential(config.FederationCredentialRekey{
		FromProjectUID: localUID,
		ToProjectUID:   hubUID,
		Expected:       manual,
		Replacement:    replacement,
	}))

	credentials, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.NotContains(t, credentials.Projects, localUID)
	assert.Equal(t, replacement, credentials.Projects[hubUID])
	assert.Len(t, credentials.Projects, 1)
}
