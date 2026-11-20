package federation_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/federationprovider"
)

type intentChangeFixture struct {
	store       db.Storage
	credentials config.FederationManagedCredentialStore
	hub         *fakeHub
	catalog     config.CatalogDaemonConfig
	mapping     config.FederationProjectConfig
	log         string
}

func newIntentChangeFixture(t *testing.T, intent federationprovider.Intent) *intentChangeFixture {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_TEST_RECONCILE_PROVIDER", "1")
	t.Setenv("KATA_TEST_PROVIDER_INTENT_CHANGE", "1")
	t.Setenv("KATA_TEST_PROVIDER_DECISION", "ready")
	t.Setenv("KATA_TEST_PROVIDER_RELEASE_DECISION", "released")
	log := filepath.Join(t.TempDir(), "provider-requests.jsonl")
	t.Setenv("KATA_TEST_PROVIDER_LOG", log)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"project_id": 42, "project_uid": hubProjectUID, "project_name": "hub-project",
			"replay_horizon_event_id": 9, "baseline_through_event_id": 12,
		}))
	}))
	t.Cleanup(server.Close)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	executable, err := os.Executable()
	require.NoError(t, err)
	catalog := config.CatalogDaemonConfig{Name: "team-hub", URL: server.URL + "/tasks"}
	return &intentChangeFixture{
		store: openReconcileStore(t), credentials: config.DefaultFederationCredentialStore(), hub: newFakeHub(),
		catalog: catalog, log: log,
		mapping: config.FederationProjectConfig{
			Hub: catalog.Name, SpokeProject: "spoke-project", HubProject: "hub-project", Intent: intent,
			CredentialProvider: []string{executable, "-test.run=^TestReconcileProviderProcess$"},
		},
	}
}

func (f *intentChangeFixture) reconcile(t *testing.T, intent federationprovider.Intent) error {
	t.Helper()
	f.mapping.Intent = intent
	return federation.ReconcileMapping(t.Context(), f.store, f.credentials, f.hub, f.catalog, f.mapping, nil)
}

func (f *intentChangeFixture) saved(t *testing.T) config.FederationManagedCredentialReservation {
	t.Helper()
	saved, found, err := f.credentials.FindManagedFederationCredential(t.Context(), f.mapping.SpokeProject)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, saved.Credential.Provider)
	return saved
}

func (f *intentChangeFixture) binding(t *testing.T) (db.Project, db.FederationBinding) {
	t.Helper()
	project, err := f.store.ProjectByName(t.Context(), f.mapping.SpokeProject)
	require.NoError(t, err)
	binding, err := f.store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	return project, binding
}

func (f *intentChangeFixture) requests(t *testing.T) []federationprovider.Request {
	t.Helper()
	file, err := os.Open(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	var out []federationprovider.Request
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var request federationprovider.Request
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &request))
		out = append(out, request)
	}
	require.NoError(t, scanner.Err())
	return out
}

func TestReconcileProviderUpgradesReadOnlyReplicaInPlace(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentReadOnly)
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))
	before := f.saved(t)
	project, binding := f.binding(t)
	require.False(t, binding.PushEnabled)
	require.NoError(t, f.store.AdvanceFederationPullCursor(t.Context(), project.ID, 30))

	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))

	after := f.saved(t)
	upgradedProject, upgraded := f.binding(t)
	assert.Equal(t, project.ID, upgradedProject.ID, "the local project is kept")
	assert.Equal(t, hubProjectUID, upgradedProject.UID)
	assert.True(t, upgraded.PushEnabled, "collaborate capabilities turn push on")
	assert.Equal(t, int64(30), upgraded.PullCursorEventID, "the binding keeps its pull progress")
	assert.Equal(t, federationprovider.IntentCollaborate, after.Credential.Provider.Intent)
	assert.Equal(t, federationprovider.StatusReady, after.Credential.Provider.Status)
	assert.NotEqual(t, before.Credential.Provider.RequestID, after.Credential.Provider.RequestID)
	assert.False(t, before.Credential.Token == after.Credential.Token, "the new request approves a new token")
	assert.Equal(t, "claim,pull,push", after.Credential.Capabilities)
	assert.NotEqual(t, before.Credential.Actor, after.Credential.Actor, "the new approval names a new actor")
	assert.Equal(t, after.Credential.Actor, upgraded.Actor, "the new actor replaces the bound actor")
	assert.Equal(t, binding.Actor, after.Credential.Provider.PreviousActor)
	issue, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "First local task", Author: "someone"})
	require.NoError(t, err)
	assert.Equal(t, after.Credential.Actor, issue.Author, "local changes are authored as the new actor")

	requests := f.requests(t)
	require.Len(t, requests, 3)
	original := before.Credential.Provider
	assert.Equal(t, "authorize", requests[0].Operation)
	assert.Equal(t, "release", requests[1].Operation)
	assert.Equal(t, original.RequestID, requests[1].RequestID)
	assert.Equal(t, federationprovider.IntentReadOnly, requests[1].Intent, "release names the request it ends")
	assert.Equal(t, "authorize", requests[2].Operation)
	assert.Equal(t, after.Credential.Provider.RequestID, requests[2].RequestID)
	assert.Equal(t, federationprovider.IntentCollaborate, requests[2].Intent)
	assert.Equal(t, original.LocalProjectUID, requests[2].LocalProjectUID, "the new request names the same local project")
	assert.Equal(t, original.SpokeInstanceUID, requests[2].SpokeInstanceUID)
	assert.Equal(t, requests[0].Project, requests[2].Project)

	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	assert.Len(t, f.requests(t), 3, "a ready request is reused after restart")
	assert.Zero(t, f.hub.resolveCalls)
	assert.Empty(t, f.hub.enrollmentCalls)
	_, restarted := f.binding(t)
	assert.Equal(t, after.Credential.Actor, restarted.Actor)
}

func TestReconcileProviderDowngradeWaitsForLocalWork(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentCollaborate)
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	before := f.saved(t)
	project, binding := f.binding(t)
	require.True(t, binding.PushEnabled)
	issue, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Unpushed local task", Author: "someone"})
	require.NoError(t, err)

	assert.Equal(t, binding.Actor, issue.Author)
	err = f.reconcile(t, federationprovider.IntentReadOnly)
	require.ErrorIs(t, err, federation.ErrBindingConflict, "an unaccepted local change blocks the downgrade")
	assert.Contains(t, err.Error(), "they stay on this computer and are not dropped", "the person learns the changes are kept")
	assert.Contains(t, err.Error(), "set the earlier intent")
	assert.Len(t, f.requests(t), 1, "a blocked downgrade does not contact the provider")
	_, blocked := f.binding(t)
	assert.True(t, blocked.PushEnabled, "the replica keeps collaborating")
	assert.True(t, before.Credential.Equal(f.saved(t).Credential), "the approved request is kept")

	accepted, err := f.store.MaxLocalOriginEventID(t.Context(), project.ID)
	require.NoError(t, err)
	require.NoError(t, f.store.AdvanceFederationPushCursor(t.Context(), project.ID, accepted))
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))

	after := f.saved(t)
	downgradedProject, downgraded := f.binding(t)
	assert.Equal(t, project.ID, downgradedProject.ID)
	assert.False(t, downgraded.PushEnabled, "read-only capabilities turn push off")
	assert.Equal(t, accepted, downgraded.PushCursorEventID)
	_, err = f.store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err, "the downgrade keeps local tasks")
	assert.Equal(t, federationprovider.IntentReadOnly, after.Credential.Provider.Intent)
	assert.Equal(t, "pull", after.Credential.Capabilities)
	assert.NotEqual(t, binding.Actor, after.Credential.Actor)
	assert.Equal(t, after.Credential.Actor, downgraded.Actor, "the new actor replaces the bound actor")
	kept, err := f.store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, binding.Actor, kept.Author, "accepted changes keep their author")
	requests := f.requests(t)
	require.Len(t, requests, 3)
	assert.Equal(t, []string{"authorize", "release", "authorize"}, []string{requests[0].Operation, requests[1].Operation, requests[2].Operation})
	assert.Equal(t, before.Credential.Provider.RequestID, requests[1].RequestID)
	assert.Equal(t, federationprovider.IntentReadOnly, requests[2].Intent)
}

func TestReconcileProviderDeniedIntentChangeKeepsReplica(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentReadOnly)
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))
	project, _ := f.binding(t)

	t.Setenv("KATA_TEST_PROVIDER_DECISION", "denied")
	err := f.reconcile(t, federationprovider.IntentCollaborate)
	require.Error(t, err)
	assert.NotErrorIs(t, err, federation.ErrConfigurationConflict)
	denied := f.saved(t)
	assert.Equal(t, federationprovider.StatusDenied, denied.Credential.Provider.Status)
	keptProject, kept := f.binding(t)
	assert.Equal(t, project.ID, keptProject.ID, "a denied request keeps the local project")
	assert.Equal(t, hubProjectUID, kept.HubProjectUID, "a denied request keeps the binding")
	assert.False(t, kept.PushEnabled)

	// Restoring the earlier intent asks again for that access.
	t.Setenv("KATA_TEST_PROVIDER_DECISION", "ready")
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))
	restored := f.saved(t)
	assert.Equal(t, federationprovider.IntentReadOnly, restored.Credential.Provider.Intent)
	assert.Equal(t, federationprovider.StatusReady, restored.Credential.Provider.Status)
	requests := f.requests(t)
	require.Len(t, requests, 5)
	assert.Equal(t, "release", requests[3].Operation)
	assert.Equal(t, denied.Credential.Provider.RequestID, requests[3].RequestID, "the denied request is released before the next one")
	assert.Equal(t, restored.Credential.Provider.RequestID, requests[4].RequestID)
}

func TestReconcileProviderIntentChangeRefusesAnotherHubProject(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentReadOnly)
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))
	project, before := f.binding(t)

	t.Setenv("KATA_TEST_PROVIDER_PROJECT_UID", recreatedProjectUID)
	err := f.reconcile(t, federationprovider.IntentCollaborate)
	require.ErrorIs(t, err, federation.ErrBindingConflict)
	pending := f.saved(t)
	assert.Empty(t, pending.Credential.Provider.Status, "approval for another project is not saved")
	assert.Equal(t, hubProjectUID, pending.Credential.Provider.HubProjectUID)
	keptProject, kept := f.binding(t)
	assert.Equal(t, project, keptProject)
	assert.Equal(t, before, kept)

	t.Setenv("KATA_TEST_PROVIDER_PROJECT_UID", "")
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	assert.Equal(t, pending.Credential.Provider.RequestID, f.saved(t).Credential.Provider.RequestID, "the retry reuses the saved request")
	_, upgraded := f.binding(t)
	assert.True(t, upgraded.PushEnabled)
	assert.Equal(t, f.saved(t).Credential.Actor, upgraded.Actor)
}

func TestReconcileProviderDowngradeRefusesExternalRoot(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentCollaborate)
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	before := f.saved(t)
	project, _ := f.binding(t)
	issue, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Bound root", Author: "someone"})
	require.NoError(t, err)
	_, _, err = f.store.CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "notes", ExternalRootKey: "root-key",
		ExternalAccountKey: "account-key", Actor: "someone", ReceiveCommentsAfter: time.Now().UTC(),
	})
	require.NoError(t, err)
	accepted, err := f.store.MaxLocalOriginEventID(t.Context(), project.ID)
	require.NoError(t, err)
	require.NoError(t, f.store.AdvanceFederationPushCursor(t.Context(), project.ID, accepted))

	err = f.reconcile(t, federationprovider.IntentReadOnly)
	require.ErrorIs(t, err, federation.ErrBindingConflict)
	assert.Contains(t, err.Error(), "active external root binding")
	assert.Len(t, f.requests(t), 1, "a refused downgrade does not contact the provider")
	_, kept := f.binding(t)
	assert.True(t, kept.PushEnabled)
	assert.True(t, before.Credential.Equal(f.saved(t).Credential))
}

// A claim request already on its way to the hub when the downgrade starts can
// be granted there before Kata caches it. The downgrade waits for that request
// to finish, so it sees the claim and keeps the replica collaborating instead
// of releasing the enrollment that could release the claim.
func TestReconcileProviderDowngradeWaitsForClaimInFlight(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentCollaborate)
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	before := f.saved(t)
	project, binding := f.binding(t)
	issue, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Claimed elsewhere", Author: "someone"})
	require.NoError(t, err)
	accepted, err := f.store.MaxLocalOriginEventID(t.Context(), project.ID)
	require.NoError(t, err)
	require.NoError(t, f.store.AdvanceFederationPushCursor(t.Context(), project.ID, accepted))

	key := federationcoord.Key(f.store.InstanceUID(), project.ID)
	finishClaim, err := federationcoord.BeginSync(t.Context(), key, f.store, project.ID)
	require.NoError(t, err)
	claimFinished := false
	defer func() {
		if !claimFinished {
			finishClaim()
		}
	}()
	downgrade := f.mapping
	downgrade.Intent = federationprovider.IntentReadOnly
	done := make(chan error, 1)
	go func() {
		done <- federation.ReconcileMapping(t.Context(), f.store, f.credentials, f.hub, f.catalog, downgrade, nil)
	}()
	// A waiting rebind makes new transport admission report busy.
	for {
		select {
		case err := <-done:
			require.Failf(t, "downgrade did not wait for the claim request", "reconcile returned %v", err)
		default:
		}
		release, err := federationcoord.TryBeginSync(t.Context(), key, f.store, project.ID)
		if errors.Is(err, federationcoord.ErrBusy) {
			break
		}
		require.NoError(t, err)
		release()
		runtime.Gosched()
	}

	// The hub granted the claim; its response is cached before the request ends.
	now := time.Now().UTC()
	inFlight, err := uid.New()
	require.NoError(t, err)
	require.NoError(t, f.store.UpsertClaimCache(t.Context(), db.IssueClaim{
		ClaimUID: inFlight, ProjectID: project.ID, IssueUID: issue.UID,
		Holder: binding.Actor, HolderInstanceUID: f.store.InstanceUID(), ClientKind: "cli",
		ClaimKind: "hard", AcquiredAt: now, UpdatedAt: now, Revision: 1,
	}))
	claimFinished = true
	finishClaim()

	err = <-done
	require.ErrorIs(t, err, federation.ErrBindingConflict, "the claim granted during the downgrade blocks it")
	assert.Len(t, f.requests(t), 1, "a blocked downgrade does not release the enrollment")
	_, kept := f.binding(t)
	assert.True(t, kept.PushEnabled, "the replica keeps collaborating so the claim can be released")
	assert.True(t, before.Credential.Equal(f.saved(t).Credential))

	require.NoError(t, f.store.ApplyClaimStatus(t.Context(), project.ID, issue.UID, db.ClaimStatus{Held: false, HubNow: now.Add(time.Second)}))
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))
	_, downgraded := f.binding(t)
	assert.False(t, downgraded.PushEnabled)
}

// A downgrade that turned push off and then failed to release keeps the old
// credential, which still forwards claims. The retry refuses to release while
// a claim taken in between is live.
func TestReconcileProviderDowngradeRetryRechecksClaims(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentCollaborate)
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	before := f.saved(t)
	project, binding := f.binding(t)
	issue, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Claimed between attempts", Author: "someone"})
	require.NoError(t, err)
	accepted, err := f.store.MaxLocalOriginEventID(t.Context(), project.ID)
	require.NoError(t, err)
	require.NoError(t, f.store.AdvanceFederationPushCursor(t.Context(), project.ID, accepted))

	t.Setenv("KATA_TEST_PROVIDER_RELEASE_DECISION", "denied")
	require.Error(t, f.reconcile(t, federationprovider.IntentReadOnly))
	_, stopped := f.binding(t)
	require.False(t, stopped.PushEnabled, "the failed attempt already turned push off")
	require.True(t, before.Credential.Equal(f.saved(t).Credential), "an unconfirmed release keeps the old credential")

	now := time.Now().UTC()
	between, err := uid.New()
	require.NoError(t, err)
	require.NoError(t, f.store.UpsertClaimCache(t.Context(), db.IssueClaim{
		ClaimUID: between, ProjectID: project.ID, IssueUID: issue.UID,
		Holder: binding.Actor, HolderInstanceUID: f.store.InstanceUID(), ClientKind: "cli",
		ClaimKind: "hard", AcquiredAt: now, UpdatedAt: now, Revision: 1,
	}))
	t.Setenv("KATA_TEST_PROVIDER_RELEASE_DECISION", "released")
	err = f.reconcile(t, federationprovider.IntentReadOnly)
	require.ErrorIs(t, err, federation.ErrBindingConflict, "the live claim blocks the retry")
	requests := f.requests(t)
	assert.Len(t, requests, 2, "the retry does not release the enrollment again")
	assert.True(t, before.Credential.Equal(f.saved(t).Credential))

	require.NoError(t, f.store.ApplyClaimStatus(t.Context(), project.ID, issue.UID, db.ClaimStatus{Held: false, HubNow: now.Add(time.Second)}))
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))
	assert.Equal(t, federationprovider.IntentReadOnly, f.saved(t).Credential.Provider.Intent)
}

// A release whose reply never arrived may have revoked the old approval.
// Restoring the earlier intent must not reuse it: Kata repeats the release
// and asks for a new approval for the restored intent.
func TestReconcileProviderRestoredIntentAfterUnconfirmedReleaseRequestsNewApproval(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentCollaborate)
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	before := f.saved(t)

	t.Setenv("KATA_TEST_PROVIDER_RELEASE_DECISION", "exit-two")
	require.Error(t, f.reconcile(t, federationprovider.IntentReadOnly))
	t.Setenv("KATA_TEST_PROVIDER_RELEASE_DECISION", "released")
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))

	requests := f.requests(t)
	require.Len(t, requests, 4)
	assert.Equal(t, "release", requests[2].Operation)
	assert.Equal(t, before.Credential.Provider.RequestID, requests[2].RequestID)
	assert.Equal(t, "authorize", requests[3].Operation)
	assert.Equal(t, federationprovider.IntentCollaborate, requests[3].Intent)
	after := f.saved(t)
	assert.NotEqual(t, before.Credential.Provider.RequestID, after.Credential.Provider.RequestID)
	assert.Equal(t, requests[3].RequestID, after.Credential.Provider.RequestID)
	_, binding := f.binding(t)
	assert.True(t, binding.PushEnabled)
}

// A provider that is unavailable when the release is repeated has still not
// said whether the old approval works, so Kata keeps trying to release it.
func TestReconcileProviderUnavailableReleaseKeepsReleasePending(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentCollaborate)
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	before := f.saved(t)

	t.Setenv("KATA_TEST_PROVIDER_RELEASE_DECISION", "exit-two")
	require.Error(t, f.reconcile(t, federationprovider.IntentReadOnly))
	t.Setenv("KATA_TEST_PROVIDER_RELEASE_DECISION", "unavailable")
	require.Error(t, f.reconcile(t, federationprovider.IntentCollaborate))
	t.Setenv("KATA_TEST_PROVIDER_RELEASE_DECISION", "released")
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))

	requests := f.requests(t)
	require.Len(t, requests, 5)
	assert.Equal(t, "release", requests[3].Operation)
	assert.Equal(t, before.Credential.Provider.RequestID, requests[3].RequestID)
	assert.Equal(t, "authorize", requests[4].Operation)
	assert.Equal(t, requests[4].RequestID, f.saved(t).Credential.Provider.RequestID)
}

// A provider that answers a release with a refusal still holds the old
// approval, so restoring the earlier intent keeps using it.
func TestReconcileProviderRestoredIntentAfterRefusedReleaseKeepsApproval(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentCollaborate)
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	before := f.saved(t)

	t.Setenv("KATA_TEST_PROVIDER_RELEASE_DECISION", "denied")
	require.Error(t, f.reconcile(t, federationprovider.IntentReadOnly))
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))

	assert.Len(t, f.requests(t), 2, "restoring the intent sends no new request")
	assert.True(t, before.Credential.Equal(f.saved(t).Credential))
	_, binding := f.binding(t)
	assert.True(t, binding.PushEnabled)
}
